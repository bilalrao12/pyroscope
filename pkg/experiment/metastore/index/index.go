package index

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"

	metastorev1 "github.com/grafana/pyroscope/api/gen/proto/go/metastore/v1"
	"github.com/grafana/pyroscope/pkg/experiment/metastore/index/store"
)

var ErrBlockExists = fmt.Errorf("block already exists")

type Store interface {
	CreateBuckets(*bbolt.Tx) error
	StoreBlock(*bbolt.Tx, store.PartitionKey, *metastorev1.BlockMeta) error
	DeleteBlockList(*bbolt.Tx, store.PartitionKey, *metastorev1.BlockList) error

	ListPartitions(*bbolt.Tx) []store.PartitionKey
	ListShards(*bbolt.Tx, store.PartitionKey) []uint32
	ListTenants(tx *bbolt.Tx, p store.PartitionKey, shard uint32) []string
	ListBlocks(tx *bbolt.Tx, p store.PartitionKey, shard uint32, tenant string) []*metastorev1.BlockMeta
}

type Index struct {
	config *Config

	partitionMu      sync.Mutex
	loadedPartitions map[cacheKey]*indexPartition
	allPartitions    []*PartitionMeta

	store  Store
	logger log.Logger
}

type Config struct {
	PartitionDuration     time.Duration `yaml:"partition_duration"`
	PartitionCacheSize    int           `yaml:"partition_cache_size"`
	QueryLookaroundPeriod time.Duration `yaml:"query_lookaround_period"`
}

func (cfg *Config) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	f.DurationVar(&cfg.PartitionDuration, prefix+"partition-duration", DefaultConfig.PartitionDuration, "")
	f.IntVar(&cfg.PartitionCacheSize, prefix+"partition-cache-size", DefaultConfig.PartitionCacheSize, "How many partitions to keep loaded in memory.")
	f.DurationVar(&cfg.QueryLookaroundPeriod, prefix+"query-lookaround-period", DefaultConfig.QueryLookaroundPeriod, "")
}

var DefaultConfig = Config{
	PartitionDuration:     24 * time.Hour,
	PartitionCacheSize:    7,
	QueryLookaroundPeriod: time.Hour,
}

type indexPartition struct {
	meta       *PartitionMeta
	accessedAt time.Time
	shards     map[uint32]*indexShard
}

type indexShard struct {
	blocks map[string]*metastorev1.BlockMeta
}

type cacheKey struct {
	partitionKey store.PartitionKey
	tenant       string
}

// NewIndex initializes a new metastore index.
//
// The index provides access to block metadata. The data is partitioned by time, shard and tenant. Partition identifiers
// contain the time period referenced by partitions, e.g., "20240923T16.1h" refers to a partition for the 1-hour period
// between 2024-09-23T16:00:00.000Z and 2024-09-23T16:59:59.999Z.
//
// Partitions are mostly transparent for the end user, though PartitionMeta is at times used externally. Partition
// durations are configurable (at application level).
//
// The index requires a backing Store for loading data in memory. Data is loaded directly via LoadPartitions() or when
// looking up blocks with FindBlock() or FindBlocksInRange().
func NewIndex(logger log.Logger, store Store, cfg *Config) *Index {
	// A fixed cache size gives us bounded memory footprint, however changes to the partition duration could reduce
	// the cache effectiveness.
	// TODO (aleks-p):
	//  - resize the cache at runtime when the config changes
	//  - consider auto-calculating the cache size to ensure we hold data for e.g., the last 24 hours
	return &Index{
		loadedPartitions: make(map[cacheKey]*indexPartition, cfg.PartitionCacheSize),
		allPartitions:    make([]*PartitionMeta, 0),
		store:            store,
		logger:           logger,
		config:           cfg,
	}
}

func NewStore() *store.IndexStore {
	return store.NewIndexStore()
}

// LoadPartitions reads all partitions from the backing store and loads the recent ones in memory.
func (i *Index) LoadPartitions(tx *bbolt.Tx) {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()

	i.allPartitions = i.allPartitions[:0]
	clear(i.loadedPartitions)
	for _, key := range i.store.ListPartitions(tx) {
		pMeta := i.loadPartitionMeta(tx, key)
		level.Info(i.logger).Log(
			"msg", "loaded metastore index partition",
			"key", key,
			"ts", pMeta.Ts.Format(time.RFC3339),
			"duration", pMeta.Duration,
			"tenants", strings.Join(pMeta.Tenants, ","))
		i.allPartitions = append(i.allPartitions, pMeta)

		// load the currently active partition
		if pMeta.contains(time.Now().UTC().UnixMilli()) {
			i.loadEntirePartition(tx, pMeta)
		}
	}
	level.Info(i.logger).Log("msg", "loaded metastore index partitions", "count", len(i.allPartitions))

	i.sortPartitions()
}

func (i *Index) loadPartitionMeta(tx *bbolt.Tx, key store.PartitionKey) *PartitionMeta {
	t, dur, _ := key.Parse()
	pMeta := &PartitionMeta{
		Key:       key,
		Ts:        t,
		Duration:  dur,
		Tenants:   make([]string, 0),
		tenantMap: make(map[string]struct{}),
	}
	for _, s := range i.store.ListShards(tx, key) {
		for _, t := range i.store.ListTenants(tx, key, s) {
			pMeta.AddTenant(t)
		}
	}
	return pMeta
}

// ForEachPartition executes the given function concurrently for each partition. It will be called for all partitions,
// regardless if they are fully loaded in memory or not.
func (i *Index) ForEachPartition(ctx context.Context, fn func(meta *PartitionMeta) error) error {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()

	g, ctx := errgroup.WithContext(ctx)
	for _, meta := range i.allPartitions {
		g.Go(func() error {
			return fn(meta)
		})
	}
	err := g.Wait()
	if err != nil {
		level.Error(i.logger).Log("msg", "error during partition iteration", "err", err)
		return err
	}
	return nil
}

func (i *Index) loadEntirePartition(tx *bbolt.Tx, meta *PartitionMeta) {
	for _, s := range i.store.ListShards(tx, meta.Key) {
		for _, t := range i.store.ListTenants(tx, meta.Key, s) {
			cKey := cacheKey{
				partitionKey: meta.Key,
				tenant:       t,
			}
			p, ok := i.loadedPartitions[cKey]
			if !ok {
				p = &indexPartition{
					meta:       meta,
					accessedAt: time.Now(),
					shards:     make(map[uint32]*indexShard),
				}
				i.loadedPartitions[cKey] = p
			}
			sh, ok := p.shards[s]
			if !ok {
				sh = &indexShard{
					blocks: make(map[string]*metastorev1.BlockMeta),
				}
				p.shards[s] = sh
			}
			for _, b := range i.store.ListBlocks(tx, meta.Key, s, t) {
				sh.blocks[b.Id] = b
			}
		}
	}
}

func (i *Index) getOrLoadPartition(tx *bbolt.Tx, meta *PartitionMeta, tenant string) *indexPartition {
	cKey := cacheKey{
		partitionKey: meta.Key,
		tenant:       tenant,
	}
	p, ok := i.loadedPartitions[cKey]
	if !ok {
		p = &indexPartition{
			meta:   meta,
			shards: make(map[uint32]*indexShard),
		}
		for _, s := range i.store.ListShards(tx, meta.Key) {
			sh := &indexShard{
				blocks: make(map[string]*metastorev1.BlockMeta),
			}
			p.shards[s] = sh
			for _, b := range i.store.ListBlocks(tx, meta.Key, s, tenant) {
				sh.blocks[b.Id] = b
			}
		}
		i.loadedPartitions[cKey] = p
	}
	p.accessedAt = time.Now().UTC()
	i.unloadPartitions()
	return p
}

// findPartitionMeta retrieves the partition meta for the given key.
func (i *Index) findPartitionMeta(key store.PartitionKey) *PartitionMeta {
	for _, p := range i.allPartitions {
		if p.Key == key {
			return p
		}
	}
	return nil
}

func (i *Index) InsertBlock(tx *bbolt.Tx, b *metastorev1.BlockMeta) error {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	if x := i.findBlock(tx, b.Shard, b.TenantId, b.Id); x != nil {
		return ErrBlockExists
	}
	i.insertBlock(tx, b)
	pk := store.CreatePartitionKey(b.Id, i.config.PartitionDuration)
	return i.store.StoreBlock(tx, pk, b)
}

func (i *Index) InsertBlockNoCheckNoPersist(tx *bbolt.Tx, b *metastorev1.BlockMeta) error {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	i.insertBlock(tx, b)
	return nil
}

// insertBlock is the underlying implementation for inserting blocks. It is the caller's responsibility to enforce safe
// concurrent access. The method will create a new partition if needed.
func (i *Index) insertBlock(tx *bbolt.Tx, b *metastorev1.BlockMeta) {
	meta := i.getOrCreatePartitionMeta(b)
	p := i.getOrLoadPartition(tx, meta, b.TenantId)
	s, ok := p.shards[b.Shard]
	if !ok {
		s = &indexShard{
			blocks: make(map[string]*metastorev1.BlockMeta),
		}
		p.shards[b.Shard] = s
	}
	_, ok = s.blocks[b.Id]
	if !ok {
		s.blocks[b.Id] = b
	}
}

func (i *Index) getOrCreatePartitionMeta(b *metastorev1.BlockMeta) *PartitionMeta {
	key := store.CreatePartitionKey(b.Id, i.config.PartitionDuration)
	meta := i.findPartitionMeta(key)

	if meta == nil {
		ts, duration, _ := key.Parse()
		meta = &PartitionMeta{
			Key:       key,
			Ts:        ts,
			Duration:  duration,
			Tenants:   make([]string, 0),
			tenantMap: make(map[string]struct{}),
		}
		i.allPartitions = append(i.allPartitions, meta)
		i.sortPartitions()
	}

	if b.TenantId != "" {
		meta.AddTenant(b.TenantId)
	} else {
		for _, ds := range b.Datasets {
			meta.AddTenant(ds.TenantId)
		}
	}

	return meta
}

func (i *Index) getOrCreatePartitionMetaForCacheKey(k cacheKey) *PartitionMeta {
	meta := i.findPartitionMeta(k.partitionKey)
	if meta == nil {
		ts, duration, _ := k.partitionKey.Parse()
		meta = &PartitionMeta{
			Key:       k.partitionKey,
			Ts:        ts,
			Duration:  duration,
			Tenants:   make([]string, 0),
			tenantMap: make(map[string]struct{}),
		}
		i.allPartitions = append(i.allPartitions, meta)
		i.sortPartitions()
	}
	return meta
}

// FindBlock tries to retrieve an existing block from the index. It will load the corresponding partition if it is not
// already loaded. Returns nil if the block cannot be found.
func (i *Index) FindBlock(tx *bbolt.Tx, shardNum uint32, tenant string, blockId string) *metastorev1.BlockMeta {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	return i.findBlock(tx, shardNum, tenant, blockId)
}

func (i *Index) FindBlocks(tx *bbolt.Tx, list *metastorev1.BlockList) []*metastorev1.BlockMeta {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()

	pk := make(map[store.PartitionKey]struct{})
	left := make(map[string]struct{})
	for _, block := range list.Blocks {
		pk[store.CreatePartitionKey(block, i.config.PartitionDuration)] = struct{}{}
		left[block] = struct{}{}
	}

	found := make([]*metastorev1.BlockMeta, 0, len(list.Blocks))
	for k := range pk {
		meta := i.findPartitionMeta(k)
		if meta == nil {
			continue
		}
		p := i.getOrLoadPartition(tx, meta, list.Tenant)
		s, _ := p.shards[list.Shard]
		if s == nil {
			continue
		}
		for b := range left {
			if block := s.blocks[b]; block != nil {
				found = append(found, block)
				delete(left, b)
			}
		}
	}

	return found
}

func (i *Index) findBlock(tx *bbolt.Tx, shardNum uint32, tenant string, blockId string) *metastorev1.BlockMeta {
	key := store.CreatePartitionKey(blockId, i.config.PartitionDuration)

	// first try the currently mapped partition
	b := i.findBlockInPartition(tx, key, shardNum, tenant, blockId)
	if b != nil {
		return b
	}

	// try other partitions that could contain the block
	t := ulid.Time(ulid.MustParse(blockId).Time()).UTC().UnixMilli()
	for _, p := range i.allPartitions {
		if p.contains(t) {
			b := i.findBlockInPartition(tx, p.Key, shardNum, tenant, blockId)
			if b != nil {
				return b
			}
		}
	}
	return nil
}

func (i *Index) findBlockInPartition(tx *bbolt.Tx, key store.PartitionKey, shard uint32, tenant string, blockId string) *metastorev1.BlockMeta {
	meta := i.findPartitionMeta(key)
	if meta == nil {
		return nil
	}

	p := i.getOrLoadPartition(tx, meta, tenant)

	s, _ := p.shards[shard]
	if s == nil {
		return nil
	}

	b, _ := s.blocks[blockId]

	return b
}

// FindBlocksInRange retrieves all blocks that might contain data for the given time range and tenants.
//
// It is not enough to scan for partition keys that fall in the given time interval. Partitions are built on top of
// block identifiers which refer to the moment a block was created and not to the timestamps of the profiles contained
// within the block (min_time, max_time). This method works around this by including blocks from adjacent partitions.
func (i *Index) FindBlocksInRange(tx *bbolt.Tx, start, end int64, tenants map[string]struct{}) []*metastorev1.BlockMeta {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	startWithLookaround := start - i.config.QueryLookaroundPeriod.Milliseconds()
	endWithLookaround := end + i.config.QueryLookaroundPeriod.Milliseconds()

	blocks := make([]*metastorev1.BlockMeta, 0)

	for _, meta := range i.allPartitions { // TODO aleks-p: consider using binary search to find a good starting point
		if meta.overlaps(startWithLookaround, endWithLookaround) {
			for t := range tenants {
				if !meta.HasTenant(t) {
					continue
				}
				p := i.getOrLoadPartition(tx, meta, t)
				tenantBlocks := i.collectTenantBlocks(p, start, end)
				blocks = append(blocks, tenantBlocks...)

				// return mixed blocks as well, we rely on the caller to filter out the data per tenant / service
				p = i.getOrLoadPartition(tx, meta, "")
				tenantBlocks = i.collectTenantBlocks(p, start, end)
				blocks = append(blocks, tenantBlocks...)
			}
		}
	}

	return blocks
}

func (i *Index) sortPartitions() {
	slices.SortFunc(i.allPartitions, func(a, b *PartitionMeta) int {
		return a.compare(b)
	})
}

func (i *Index) collectTenantBlocks(p *indexPartition, start, end int64) []*metastorev1.BlockMeta {
	blocks := make([]*metastorev1.BlockMeta, 0)
	for _, s := range p.shards {
		for _, block := range s.blocks {
			if start < block.MaxTime && end >= block.MinTime {
				clone := block.CloneVT()
				blocks = append(blocks, clone)
			}
		}
	}
	return blocks
}

// ReplaceBlocks removes source blocks from the index and inserts replacement blocks into the index. The intended usage
// is for block compaction. The replacement blocks could be added to the same or a different partition.
func (i *Index) ReplaceBlocks(tx *bbolt.Tx, compacted *metastorev1.CompactedBlocks) error {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	if err := i.insertBlocks(tx, compacted.NewBlocks); err != nil {
		return err
	}
	return i.deleteBlockList(tx, compacted.SourceBlocks)
}

func (i *Index) ReplaceBlocksNoCheckNoPersist(tx *bbolt.Tx, compacted *metastorev1.CompactedBlocks) error {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	for _, b := range compacted.NewBlocks {
		i.insertBlock(tx, b)
	}
	source := compacted.SourceBlocks
	for _, b := range source.Blocks {
		i.deleteBlock(source.Shard, source.Tenant, b)
	}
	return nil
}

func (i *Index) insertBlocks(tx *bbolt.Tx, blocks []*metastorev1.BlockMeta) error {
	for _, b := range blocks {
		k := store.CreatePartitionKey(b.Id, i.config.PartitionDuration)
		i.insertBlock(tx, b)
		if err := i.store.StoreBlock(tx, k, b); err != nil {
			return err
		}
	}
	return nil
}

func (i *Index) deleteBlockList(tx *bbolt.Tx, list *metastorev1.BlockList) error {
	partitions := make(map[store.PartitionKey]*metastorev1.BlockList)
	for _, block := range list.Blocks {
		k := store.CreatePartitionKey(block, i.config.PartitionDuration)
		v := partitions[k]
		if v == nil {
			v = &metastorev1.BlockList{
				Shard:  list.Shard,
				Tenant: list.Tenant,
				Blocks: make([]string, 0, len(list.Blocks)),
			}
			partitions[k] = v
		}
		v.Blocks = append(v.Blocks, block)
	}
	for k, partitioned := range partitions {
		if err := i.store.DeleteBlockList(tx, k, partitioned); err != nil {
			return err
		}
		ck := cacheKey{partitionKey: k, tenant: list.Tenant}
		loaded := i.loadedPartitions[ck]
		if loaded == nil {
			continue
		}
		shard := loaded.shards[partitioned.Shard]
		if shard == nil {
			continue
		}
		for _, b := range partitioned.Blocks {
			delete(shard.blocks, b)
		}
	}
	return nil
}

// deleteBlock deletes a block from the index. It is the caller's responsibility to enforce safe concurrent access.
func (i *Index) deleteBlock(shard uint32, tenant string, blockId string) {
	// first try the currently mapped partition
	key := store.CreatePartitionKey(blockId, i.config.PartitionDuration)
	if ok := i.tryDelete(key, shard, tenant, blockId); ok {
		return
	}

	// now try all other possible partitions
	t := ulid.Time(ulid.MustParse(blockId).Time()).UTC().UnixMilli()

	for _, p := range i.allPartitions {
		if p.contains(t) {
			if ok := i.tryDelete(p.Key, shard, tenant, blockId); ok {
				return
			}
		}
	}
}

func (i *Index) tryDelete(key store.PartitionKey, shard uint32, tenant string, blockId string) bool {
	meta := i.findPartitionMeta(key)
	if meta == nil {
		return false
	}

	cKey := cacheKey{
		partitionKey: key,
		tenant:       tenant,
	}
	p, ok := i.loadedPartitions[cKey]
	if !ok {
		return false
	}

	s, ok := p.shards[shard]
	if !ok {
		return false
	}

	if s.blocks[blockId] != nil {
		delete(s.blocks, blockId)
		return true
	}

	return false
}

func (i *Index) FindPartitionMetas(blockId string) []*PartitionMeta {
	i.partitionMu.Lock()
	defer i.partitionMu.Unlock()
	ts := ulid.Time(ulid.MustParse(blockId).Time()).UTC().UnixMilli()

	metas := make([]*PartitionMeta, 0)
	for _, p := range i.allPartitions {
		if p.contains(ts) {
			metas = append(metas, p)
		}
	}
	return metas
}

func (i *Index) unloadPartitions() {
	tenantPartitions := make(map[string][]*indexPartition)
	excessPerTenant := make(map[string]int)
	for k, p := range i.loadedPartitions {
		tenantPartitions[k.tenant] = append(tenantPartitions[k.tenant], p)
		if len(tenantPartitions[k.tenant]) > i.config.PartitionCacheSize {
			excessPerTenant[k.tenant]++
		}
	}

	for t, partitions := range tenantPartitions {
		toRemove, ok := excessPerTenant[t]
		if !ok {
			continue
		}
		slices.SortFunc(partitions, func(a, b *indexPartition) int {
			return a.accessedAt.Compare(b.accessedAt)
		})
		level.Debug(i.logger).Log("msg", "unloading metastore index partitions", "tenant", t, "to_remove", len(partitions))
		for _, p := range partitions {
			if p.meta.contains(time.Now().UTC().UnixMilli()) {
				continue
			}
			level.Debug(i.logger).Log("unloading metastore index partition", "key", p.meta.Key, "accessed_at", p.accessedAt.Format(time.RFC3339))
			cKey := cacheKey{
				partitionKey: p.meta.Key,
				tenant:       t,
			}
			delete(i.loadedPartitions, cKey)
			toRemove--
			if toRemove == 0 {
				break
			}
		}
	}
}

func (i *Index) Init(tx *bbolt.Tx) error {
	return i.store.CreateBuckets(tx)
}

func (i *Index) Restore(tx *bbolt.Tx) error {
	i.LoadPartitions(tx)
	return nil
}
