package chunk

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/weaveworks/common/httpgrpc"

	"github.com/cortexproject/cortex/pkg/chunk/cache"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/extract"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/spanlogger"
	"github.com/cortexproject/cortex/pkg/util/validation"
)

var (
	ErrMetricNameLabelMissing     = errors.New("metric name label missing")
	ErrParialDeleteChunkNoOverlap = errors.New("interval for partial deletion has not overlap with chunk interval")
)

var (
	indexEntriesPerChunk = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "chunk_store_index_entries_per_chunk",
		Help:      "Number of entries written to storage per chunk.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 5),
	})
	cacheCorrupt = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "cache_corrupt_chunks_total",
		Help:      "Total count of corrupt chunks found in cache.",
	})
)

// StoreConfig specifies config for a ChunkStore
type StoreConfig struct {
	ChunkCacheConfig       cache.Config `yaml:"chunk_cache_config,omitempty"`
	WriteDedupeCacheConfig cache.Config `yaml:"write_dedupe_cache_config,omitempty"`

	CacheLookupsOlderThan time.Duration `yaml:"cache_lookups_older_than,omitempty"`

	// Limits query start time to be greater than now() - MaxLookBackPeriod, if set.
	MaxLookBackPeriod time.Duration `yaml:"max_look_back_period"`

	// Not visible in yaml because the setting shouldn't be common between ingesters and queriers
	chunkCacheStubs bool // don't write the full chunk to cache, just a stub entry
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *StoreConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.ChunkCacheConfig.RegisterFlagsWithPrefix("", "Cache config for chunks. ", f)
	f.BoolVar(&cfg.chunkCacheStubs, "store.chunk-cache-stubs", false, "If true, don't write the full chunk to cache, just a stub entry.")
	cfg.WriteDedupeCacheConfig.RegisterFlagsWithPrefix("store.index-cache-write.", "Cache config for index entry writing. ", f)

	f.DurationVar(&cfg.CacheLookupsOlderThan, "store.cache-lookups-older-than", 0, "Cache index entries older than this period. 0 to disable.")
	f.DurationVar(&cfg.MaxLookBackPeriod, "store.max-look-back-period", 0, "Limit how long back data can be queried")

	// Deprecated.
	flagext.DeprecatedFlag(f, "store.cardinality-cache-size", "DEPRECATED. Use store.index-cache-read.enable-fifocache and store.index-cache-read.fifocache.size instead.")
	flagext.DeprecatedFlag(f, "store.cardinality-cache-validity", "DEPRECATED. Use store.index-cache-read.enable-fifocache and store.index-cache-read.fifocache.duration instead.")
}

// store implements Store
type store struct {
	cfg StoreConfig

	index  IndexClient
	chunks Client
	schema Schema
	limits StoreLimits
	*Fetcher
}

func newStore(cfg StoreConfig, schema Schema, index IndexClient, chunks Client, limits StoreLimits) (Store, error) {
	fetcher, err := NewChunkFetcher(cfg.ChunkCacheConfig, cfg.chunkCacheStubs, chunks)
	if err != nil {
		return nil, err
	}

	return &store{
		cfg:     cfg,
		index:   index,
		chunks:  chunks,
		schema:  schema,
		limits:  limits,
		Fetcher: fetcher,
	}, nil
}

// Stop any background goroutines (ie in the cache.)
func (c *store) Stop() {
	c.storage.Stop()
	c.Fetcher.Stop()
}

// Put implements ChunkStore
func (c *store) Put(ctx context.Context, chunks []Chunk) error {
	for _, chunk := range chunks {
		if err := c.PutOne(ctx, chunk.From, chunk.Through, chunk); err != nil {
			return err
		}
	}
	return nil
}

// PutOne implements ChunkStore
func (c *store) PutOne(ctx context.Context, from, through model.Time, chunk Chunk) error {
	log, ctx := spanlogger.New(ctx, "ChunkStore.PutOne")
	chunks := []Chunk{chunk}

	err := c.storage.PutChunks(ctx, chunks)
	if err != nil {
		return err
	}

	if cacheErr := c.writeBackCache(ctx, chunks); cacheErr != nil {
		level.Warn(log).Log("msg", "could not store chunks in chunk cache", "err", cacheErr)
	}

	writeReqs, err := c.calculateIndexEntries(chunk.UserID, from, through, chunk)
	if err != nil {
		return err
	}

	return c.index.BatchWrite(ctx, writeReqs)
}

// calculateIndexEntries creates a set of batched WriteRequests for all the chunks it is given.
func (c *store) calculateIndexEntries(userID string, from, through model.Time, chunk Chunk) (WriteBatch, error) {
	seenIndexEntries := map[string]struct{}{}

	metricName := chunk.Metric.Get(labels.MetricName)
	if metricName == "" {
		return nil, ErrMetricNameLabelMissing
	}

	entries, err := c.schema.GetWriteEntries(from, through, userID, metricName, chunk.Metric, chunk.ExternalKey())
	if err != nil {
		return nil, err
	}
	indexEntriesPerChunk.Observe(float64(len(entries)))

	// Remove duplicate entries based on tableName:hashValue:rangeValue
	result := c.index.NewWriteBatch()
	for _, entry := range entries {
		key := fmt.Sprintf("%s:%s:%x", entry.TableName, entry.HashValue, entry.RangeValue)
		if _, ok := seenIndexEntries[key]; !ok {
			seenIndexEntries[key] = struct{}{}
			result.Add(entry.TableName, entry.HashValue, entry.RangeValue, entry.Value)
		}
	}
	return result, nil
}

// Get implements Store
func (c *store) Get(ctx context.Context, userID string, from, through model.Time, allMatchers ...*labels.Matcher) ([]Chunk, error) {
	log, ctx := spanlogger.New(ctx, "ChunkStore.Get")
	defer log.Span.Finish()
	level.Debug(log).Log("from", from, "through", through, "matchers", len(allMatchers))

	// Validate the query is within reasonable bounds.
	metricName, matchers, shortcut, err := c.validateQuery(ctx, userID, &from, &through, allMatchers)
	if err != nil {
		return nil, err
	} else if shortcut {
		return nil, nil
	}

	log.Span.SetTag("metric", metricName)
	return c.getMetricNameChunks(ctx, userID, from, through, matchers, metricName)
}

func (c *store) GetChunkRefs(ctx context.Context, userID string, from, through model.Time, allMatchers ...*labels.Matcher) ([][]Chunk, []*Fetcher, error) {
	return nil, nil, errors.New("not implemented")
}

// LabelValuesForMetricName retrieves all label values for a single label name and metric name.
func (c *store) LabelValuesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName, labelName string) ([]string, error) {
	log, ctx := spanlogger.New(ctx, "ChunkStore.LabelValues")
	defer log.Span.Finish()
	level.Debug(log).Log("from", from, "through", through, "metricName", metricName, "labelName", labelName)

	shortcut, err := c.validateQueryTimeRange(ctx, userID, &from, &through)
	if err != nil {
		return nil, err
	} else if shortcut {
		return nil, nil
	}

	queries, err := c.schema.GetReadQueriesForMetricLabel(from, through, userID, metricName, labelName)
	if err != nil {
		return nil, err
	}

	entries, err := c.lookupEntriesByQueries(ctx, queries)
	if err != nil {
		return nil, err
	}

	var result UniqueStrings
	for _, entry := range entries {
		_, labelValue, _, err := parseChunkTimeRangeValue(entry.RangeValue, entry.Value)
		if err != nil {
			return nil, err
		}
		result.Add(string(labelValue))
	}
	return result.Strings(), nil
}

// LabelNamesForMetricName retrieves all label names for a metric name.
func (c *store) LabelNamesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName string) ([]string, error) {
	log, ctx := spanlogger.New(ctx, "ChunkStore.LabelNamesForMetricName")
	defer log.Span.Finish()
	level.Debug(log).Log("from", from, "through", through, "metricName", metricName)

	shortcut, err := c.validateQueryTimeRange(ctx, userID, &from, &through)
	if err != nil {
		return nil, err
	} else if shortcut {
		return nil, nil
	}

	chunks, err := c.lookupChunksByMetricName(ctx, userID, from, through, nil, metricName)
	if err != nil {
		return nil, err
	}
	level.Debug(log).Log("msg", "Chunks in index", "chunks", len(chunks))

	// Filter out chunks that are not in the selected time range and keep a single chunk per fingerprint
	filtered := filterChunksByTime(from, through, chunks)
	filtered, keys := filterChunksByUniqueFingerprint(filtered)
	level.Debug(log).Log("msg", "Chunks post filtering", "chunks", len(chunks))

	// Now fetch the actual chunk data from Memcache / S3
	allChunks, err := c.FetchChunks(ctx, filtered, keys)
	if err != nil {
		level.Error(log).Log("msg", "FetchChunks", "err", err)
		return nil, err
	}
	return labelNamesFromChunks(allChunks), nil
}

func (c *store) validateQueryTimeRange(ctx context.Context, userID string, from *model.Time, through *model.Time) (bool, error) {
	//nolint:ineffassign,staticcheck //Leaving ctx even though we don't currently use it, we want to make it available for when we might need it and hopefully will ensure us using the correct context at that time
	log, ctx := spanlogger.New(ctx, "store.validateQueryTimeRange")
	defer log.Span.Finish()

	if *through < *from {
		return false, httpgrpc.Errorf(http.StatusBadRequest, "invalid query, through < from (%s < %s)", through, from)
	}

	maxQueryLength := c.limits.MaxQueryLength(userID)
	if maxQueryLength > 0 && (*through).Sub(*from) > maxQueryLength {
		return false, httpgrpc.Errorf(http.StatusBadRequest, validation.ErrQueryTooLong, (*through).Sub(*from), maxQueryLength)
	}

	now := model.Now()

	if from.After(now) {
		// time-span start is in future ... regard as legal
		level.Info(log).Log("msg", "whole timerange in future, yield empty resultset", "through", through, "from", from, "now", now)
		return true, nil
	}

	if c.cfg.MaxLookBackPeriod != 0 {
		oldestStartTime := model.Now().Add(-c.cfg.MaxLookBackPeriod)
		if oldestStartTime.After(*from) {
			*from = oldestStartTime
		}
	}

	if through.After(now.Add(5 * time.Minute)) {
		// time-span end is in future ... regard as legal
		level.Info(log).Log("msg", "adjusting end timerange from future to now", "old_through", through, "new_through", now)
		*through = now // Avoid processing future part - otherwise some schemas could fail with eg non-existent table gripes
	}

	return false, nil
}

func (c *store) validateQuery(ctx context.Context, userID string, from *model.Time, through *model.Time, matchers []*labels.Matcher) (string, []*labels.Matcher, bool, error) {
	log, ctx := spanlogger.New(ctx, "store.validateQuery")
	defer log.Span.Finish()

	shortcut, err := c.validateQueryTimeRange(ctx, userID, from, through)
	if err != nil {
		return "", nil, false, err
	}
	if shortcut {
		return "", nil, true, nil
	}

	// Check there is a metric name matcher of type equal,
	metricNameMatcher, matchers, ok := extract.MetricNameMatcherFromMatchers(matchers)
	if !ok || metricNameMatcher.Type != labels.MatchEqual {
		return "", nil, false, httpgrpc.Errorf(http.StatusBadRequest, "query must contain metric name")
	}

	return metricNameMatcher.Value, matchers, false, nil
}

func (c *store) getMetricNameChunks(ctx context.Context, userID string, from, through model.Time, allMatchers []*labels.Matcher, metricName string) ([]Chunk, error) {
	log, ctx := spanlogger.New(ctx, "ChunkStore.getMetricNameChunks")
	defer log.Finish()
	level.Debug(log).Log("from", from, "through", through, "metricName", metricName, "matchers", len(allMatchers))

	filters, matchers := util.SplitFiltersAndMatchers(allMatchers)
	chunks, err := c.lookupChunksByMetricName(ctx, userID, from, through, matchers, metricName)
	if err != nil {
		return nil, err
	}
	level.Debug(log).Log("Chunks in index", len(chunks))

	// Filter out chunks that are not in the selected time range.
	filtered := filterChunksByTime(from, through, chunks)
	level.Debug(log).Log("Chunks post filtering", len(chunks))

	maxChunksPerQuery := c.limits.MaxChunksPerQuery(userID)
	if maxChunksPerQuery > 0 && len(filtered) > maxChunksPerQuery {
		err := httpgrpc.Errorf(http.StatusBadRequest, "Query %v fetched too many chunks (%d > %d)", allMatchers, len(filtered), maxChunksPerQuery)
		level.Error(log).Log("err", err)
		return nil, err
	}

	// Now fetch the actual chunk data from Memcache / S3
	keys := keysFromChunks(filtered)
	allChunks, err := c.FetchChunks(ctx, filtered, keys)
	if err != nil {
		return nil, promql.ErrStorage{Err: err}
	}

	// Filter out chunks based on the empty matchers in the query.
	filteredChunks := filterChunksByMatchers(allChunks, filters)
	return filteredChunks, nil
}

func (c *store) lookupChunksByMetricName(ctx context.Context, userID string, from, through model.Time, matchers []*labels.Matcher, metricName string) ([]Chunk, error) {
	log, ctx := spanlogger.New(ctx, "ChunkStore.lookupChunksByMetricName")
	defer log.Finish()

	// Just get chunks for metric if there are no matchers
	if len(matchers) == 0 {
		queries, err := c.schema.GetReadQueriesForMetric(from, through, userID, metricName)
		if err != nil {
			return nil, err
		}
		level.Debug(log).Log("queries", len(queries))

		entries, err := c.lookupEntriesByQueries(ctx, queries)
		if err != nil {
			return nil, err
		}
		level.Debug(log).Log("entries", len(entries))

		chunkIDs, err := c.parseIndexEntries(ctx, entries, nil)
		if err != nil {
			return nil, err
		}
		level.Debug(log).Log("chunkIDs", len(chunkIDs))

		return c.convertChunkIDsToChunks(ctx, userID, chunkIDs)
	}

	// Otherwise get chunks which include other matchers
	incomingChunkIDs := make(chan []string)
	incomingErrors := make(chan error)
	for _, matcher := range matchers {
		go func(matcher *labels.Matcher) {
			// Lookup IndexQuery's
			var queries []IndexQuery
			var err error
			if matcher.Type != labels.MatchEqual {
				queries, err = c.schema.GetReadQueriesForMetricLabel(from, through, userID, metricName, matcher.Name)
			} else {
				queries, err = c.schema.GetReadQueriesForMetricLabelValue(from, through, userID, metricName, matcher.Name, matcher.Value)
			}
			if err != nil {
				incomingErrors <- err
				return
			}
			level.Debug(log).Log("matcher", matcher, "queries", len(queries))

			// Lookup IndexEntry's
			entries, err := c.lookupEntriesByQueries(ctx, queries)
			if err != nil {
				incomingErrors <- err
				return
			}
			level.Debug(log).Log("matcher", matcher, "entries", len(entries))

			// Convert IndexEntry's to chunk IDs, filter out non-matchers at the same time.
			chunkIDs, err := c.parseIndexEntries(ctx, entries, matcher)
			if err != nil {
				incomingErrors <- err
				return
			}
			level.Debug(log).Log("matcher", matcher, "chunkIDs", len(chunkIDs))
			incomingChunkIDs <- chunkIDs
		}(matcher)
	}

	// Receive chunkSets from all matchers
	var chunkIDs []string
	var lastErr error
	for i := 0; i < len(matchers); i++ {
		select {
		case incoming := <-incomingChunkIDs:
			if chunkIDs == nil {
				chunkIDs = incoming
			} else {
				chunkIDs = intersectStrings(chunkIDs, incoming)
			}
		case err := <-incomingErrors:
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	level.Debug(log).Log("msg", "post intersection", "chunkIDs", len(chunkIDs))

	// Convert IndexEntry's into chunks
	return c.convertChunkIDsToChunks(ctx, userID, chunkIDs)
}

func (c *store) lookupEntriesByQueries(ctx context.Context, queries []IndexQuery) ([]IndexEntry, error) {
	log, ctx := spanlogger.New(ctx, "store.lookupEntriesByQueries")
	defer log.Span.Finish()

	var lock sync.Mutex
	var entries []IndexEntry
	err := c.index.QueryPages(ctx, queries, func(query IndexQuery, resp ReadBatch) bool {
		iter := resp.Iterator()
		lock.Lock()
		for iter.Next() {
			entries = append(entries, IndexEntry{
				TableName:  query.TableName,
				HashValue:  query.HashValue,
				RangeValue: iter.RangeValue(),
				Value:      iter.Value(),
			})
		}
		lock.Unlock()
		return true
	})
	if err != nil {
		level.Error(util.WithContext(ctx, util.Logger)).Log("msg", "error querying storage", "err", err)
	}
	return entries, err
}

func (c *store) parseIndexEntries(ctx context.Context, entries []IndexEntry, matcher *labels.Matcher) ([]string, error) {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		chunkKey, labelValue, _, err := parseChunkTimeRangeValue(entry.RangeValue, entry.Value)
		if err != nil {
			return nil, err
		}

		if matcher != nil && !matcher.Matches(string(labelValue)) {
			continue
		}
		result = append(result, chunkKey)
	}
	// Return ids sorted and deduped because they will be merged with other sets.
	sort.Strings(result)
	result = uniqueStrings(result)
	return result, nil
}

func (c *store) convertChunkIDsToChunks(ctx context.Context, userID string, chunkIDs []string) ([]Chunk, error) {
	chunkSet := make([]Chunk, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		chunk, err := ParseExternalKey(userID, chunkID)
		if err != nil {
			return nil, err
		}
		chunkSet = append(chunkSet, chunk)
	}

	return chunkSet, nil
}

func (c *store) DeleteChunk(ctx context.Context, from, through model.Time, userID, chunkID string, metric labels.Labels, partiallyDeletedInterval *model.Interval) error {
	metricName := metric.Get(model.MetricNameLabel)
	if metricName == "" {
		return ErrMetricNameLabelMissing
	}

	chunkWriteEntries, err := c.schema.GetWriteEntries(from, through, userID, string(metricName), metric, chunkID)
	if err != nil {
		return errors.Wrapf(err, "when getting index entries to delete for chunkID=%s", chunkID)
	}

	return c.deleteChunk(ctx, userID, chunkID, metric, chunkWriteEntries, partiallyDeletedInterval, func(chunk Chunk) error {
		return c.PutOne(ctx, chunk.From, chunk.Through, chunk)
	})
}

func (c *store) deleteChunk(ctx context.Context,
	userID string,
	chunkID string,
	metric labels.Labels,
	chunkWriteEntries []IndexEntry,
	partiallyDeletedInterval *model.Interval,
	putChunkFunc func(chunk Chunk) error) error {

	metricName := metric.Get(model.MetricNameLabel)
	if metricName == "" {
		return ErrMetricNameLabelMissing
	}

	// if chunk is partially deleted, fetch it, slice non-deleted portion and put it to store before deleting original chunk
	if partiallyDeletedInterval != nil {
		err := c.reboundChunk(ctx, userID, chunkID, *partiallyDeletedInterval, putChunkFunc)
		if err != nil {
			return errors.Wrapf(err, "chunkID=%s", chunkID)
		}
	}

	batch := c.index.NewWriteBatch()
	for i := range chunkWriteEntries {
		batch.Delete(chunkWriteEntries[i].TableName, chunkWriteEntries[i].HashValue, chunkWriteEntries[i].RangeValue)
	}

	err := c.index.BatchWrite(ctx, batch)
	if err != nil {
		return errors.Wrapf(err, "when deleting index entries for chunkID=%s", chunkID)
	}

	err = c.chunks.DeleteChunk(ctx, chunkID)
	if err != nil {
		if err == ErrStorageObjectNotFound {
			return nil
		}
		return errors.Wrapf(err, "when deleting chunk from storage with chunkID=%s", chunkID)
	}

	return nil
}

func (c *store) reboundChunk(ctx context.Context, userID, chunkID string, partiallyDeletedInterval model.Interval, putChunkFunc func(chunk Chunk) error) error {
	chunk, err := ParseExternalKey(userID, chunkID)
	if err != nil {
		return errors.Wrap(err, "when parsing external key")
	}

	if !intervalsOverlap(model.Interval{Start: chunk.From, End: chunk.Through}, partiallyDeletedInterval) {
		return ErrParialDeleteChunkNoOverlap
	}

	chunks, err := c.Fetcher.FetchChunks(ctx, []Chunk{chunk}, []string{chunkID})
	if err != nil {
		if err == ErrStorageObjectNotFound {
			return nil
		}
		return errors.Wrap(err, "when fetching chunk from storage for slicing")
	}

	if len(chunks) != 1 {
		return fmt.Errorf("expected to get 1 chunk from storage got %d instead", len(chunks))
	}

	chunk = chunks[0]
	var newChunks []*Chunk
	if partiallyDeletedInterval.Start > chunk.From {
		newChunk, err := chunk.Slice(chunk.From, partiallyDeletedInterval.Start-1)
		if err != nil && err != ErrSliceNoDataInRange {
			return errors.Wrapf(err, "when slicing chunk for interval %d - %d", chunk.From, partiallyDeletedInterval.Start-1)
		}

		if newChunk != nil {
			newChunks = append(newChunks, newChunk)
		}
	}

	if partiallyDeletedInterval.End < chunk.Through {
		newChunk, err := chunk.Slice(partiallyDeletedInterval.End+1, chunk.Through)
		if err != nil && err != ErrSliceNoDataInRange {
			return errors.Wrapf(err, "when slicing chunk for interval %d - %d", partiallyDeletedInterval.End+1, chunk.Through)
		}

		if newChunk != nil {
			newChunks = append(newChunks, newChunk)
		}
	}

	for _, newChunk := range newChunks {
		if err := newChunk.Encode(); err != nil {
			return errors.Wrapf(err, "when encoding new chunk formed after slicing for interval %d - %d", newChunk.From, newChunk.Through)
		}

		err = putChunkFunc(*newChunk)
		if err != nil {
			return errors.Wrapf(err, "when putting new chunk formed after slicing for interval %d - %d", newChunk.From, newChunk.Through)
		}
	}

	return nil
}

func (c *store) DeleteSeriesIDs(ctx context.Context, from, through model.Time, userID string, metric labels.Labels) error {
	// SeriesID is something which is only used in SeriesStore so we need not do anything here
	return nil
}
