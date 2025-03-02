// Copyright 2023 The Kelemetry Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jaegerreader

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/storage/spanstore"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	jaegerbackend "github.com/kubewharf/kelemetry/pkg/frontend/backend"
	"github.com/kubewharf/kelemetry/pkg/frontend/clusterlist"
	transform "github.com/kubewharf/kelemetry/pkg/frontend/tf"
	tfconfig "github.com/kubewharf/kelemetry/pkg/frontend/tf/config"
	tftree "github.com/kubewharf/kelemetry/pkg/frontend/tf/tree"
	"github.com/kubewharf/kelemetry/pkg/frontend/tracecache"
	"github.com/kubewharf/kelemetry/pkg/manager"
	"github.com/kubewharf/kelemetry/pkg/util/zconstants"
)

func init() {
	manager.Global.Provide("jaeger-span-reader", manager.Ptr[Interface](&spanReader{}))
}

type Interface interface {
	spanstore.Reader
}

type options struct {
	cacheExtensions bool
}

func (options *options) Setup(fs *pflag.FlagSet) {
	fs.BoolVar(
		&options.cacheExtensions,
		"jaeger-extension-cache",
		false,
		"cache extension trace search result, otherwise trace is searched again every time result is reloaded",
	)
}

func (options *options) EnableFlag() *bool { return nil }

type spanReader struct {
	options          options
	Logger           logrus.FieldLogger
	Backend          jaegerbackend.Backend
	TraceCache       tracecache.Cache
	ClusterList      clusterlist.Lister
	Transformer      *transform.Transformer
	TransformConfigs tfconfig.Provider
}

func (reader *spanReader) Options() manager.Options        { return &reader.options }
func (reader *spanReader) Init() error                     { return nil }
func (reader *spanReader) Start(ctx context.Context) error { return nil }
func (reader *spanReader) Close(ctx context.Context) error { return nil }

func (reader *spanReader) GetServices(ctx context.Context) ([]string, error) {
	configNames := []string{
		reader.TransformConfigs.DefaultName(),
	}

	for _, name := range reader.TransformConfigs.Names() {
		if name != reader.TransformConfigs.DefaultName() {
			configNames = append(configNames, name)
		}
	}

	reader.Logger.WithField("services", configNames).Info("query display mode list")

	return configNames, nil
}

func (reader *spanReader) GetOperations(ctx context.Context, query spanstore.OperationQueryParameters) ([]spanstore.Operation, error) {
	clusterNames := reader.ClusterList.List()
	operations := make([]spanstore.Operation, 0, len(clusterNames))
	for _, verb := range clusterNames {
		operations = append(operations, spanstore.Operation{
			SpanKind: query.SpanKind,
			Name:     verb,
		})
	}

	return operations, nil
}

func (reader *spanReader) FindTraceIDs(ctx context.Context, query *spanstore.TraceQueryParameters) ([]model.TraceID, error) {
	traces, err := reader.FindTraces(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("FindTrace error: %w", err)
	}

	traceIds := make([]model.TraceID, 0, len(traces))
	for _, trace := range traces {
		if len(trace.Spans) > 0 {
			traceIds = append(traceIds, trace.Spans[0].TraceID)
		}
	}
	return traceIds, nil
}

func (reader *spanReader) FindTraces(ctx context.Context, query *spanstore.TraceQueryParameters) ([]*model.Trace, error) {
	configName := strings.TrimPrefix(query.ServiceName, "* ")
	config := reader.TransformConfigs.GetByName(configName)
	if config == nil {
		return nil, fmt.Errorf("invalid display mode %q", query.ServiceName)
	}

	reader.Logger.WithField("query", query).
		WithField("exclusive", config.UseSubtree).
		WithField("config", config.Name).
		Debug("start trace list")
	thumbnails, err := reader.Backend.List(ctx, query, config.UseSubtree)
	if err != nil {
		return nil, err
	}

	var rootKey *tftree.GroupingKey
	if rootKeyValue, ok := tftree.GroupingKeyFromMap(query.Tags); ok {
		rootKey = &rootKeyValue
	}

	mergedEntries := mergeSegments(thumbnails)

	cacheEntries := []tracecache.Entry{}
	traces := []*model.Trace{}
	for _, entry := range mergedEntries {
		cacheId := generateCacheId(config.Id)

		for _, span := range entry.spans {
			span.TraceID = cacheId
			for i := range span.References {
				span.References[i].TraceID = cacheId
			}
		}

		entry.spans = filterTimeRange(entry.spans, query.StartTimeMin, query.StartTimeMax)

		trace := &model.Trace{
			ProcessMap: []model.Trace_ProcessMapping{{
				ProcessID: "0",
				Process:   model.Process{},
			}},
			Spans: entry.spans,
		}

		displayMode := extractDisplayMode(cacheId)

		extensions := &transform.FetchExtensionsAndStoreCache{}

		if err := reader.Transformer.Transform(
			ctx, trace, rootKey, displayMode,
			extensions,
			query.StartTimeMin, query.StartTimeMax,
		); err != nil {
			return nil, fmt.Errorf("trace transformation failed: %w", err)
		}
		traces = append(traces, trace)

		identifiers := make([]json.RawMessage, len(entry.identifiers))
		for i, identifier := range entry.identifiers {
			idJson, err := json.Marshal(identifier)
			if err != nil {
				return nil, fmt.Errorf("thumbnail identifier marshal: %w", err)
			}

			identifiers[i] = json.RawMessage(idJson)
		}

		cacheEntry := tracecache.Entry{
			LowId: cacheId.Low,
			Value: tracecache.EntryValue{
				Identifiers: identifiers,
				StartTime:   query.StartTimeMin,
				EndTime:     query.StartTimeMax,
				RootObject:  rootKey,
			},
		}
		if reader.options.cacheExtensions {
			cacheEntry.Value.Extensions = extensions.Cache
		}
		cacheEntries = append(cacheEntries, cacheEntry)
	}

	if len(cacheEntries) > 0 {
		if err := reader.TraceCache.Persist(ctx, cacheEntries); err != nil {
			return nil, fmt.Errorf("cannot persist trace cache: %w", err)
		}
	}

	reader.Logger.WithField("numTraces", len(traces)).Info("query trace list")

	return traces, nil
}

func (reader *spanReader) GetTrace(ctx context.Context, cacheId model.TraceID) (*model.Trace, error) {
	entry, err := reader.TraceCache.Fetch(ctx, cacheId.Low)
	if err != nil {
		return nil, fmt.Errorf("cannot lookup trace: %w", err)
	}
	if entry == nil {
		return nil, fmt.Errorf("trace %v not found", cacheId)
	}

	aggTrace := &model.Trace{
		ProcessMap: []model.Trace_ProcessMapping{{
			ProcessID: "0",
			Process:   model.Process{},
		}},
	}

	for _, identifier := range entry.Identifiers {
		trace, err := reader.Backend.Get(ctx, identifier, cacheId, entry.StartTime, entry.EndTime)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch trace pointed by the cache: %w", err)
		}

		clipped := filterTimeRange(trace.Spans, entry.StartTime, entry.EndTime)
		aggTrace.Spans = append(aggTrace.Spans, clipped...)
	}

	var extensions transform.ExtensionProcessor = &transform.FetchExtensionsAndStoreCache{}
	if reader.options.cacheExtensions && len(entry.Extensions) > 0 {
		extensions = &transform.LoadExtensionCache{Cache: entry.Extensions}
	}

	displayMode := extractDisplayMode(cacheId)
	if err := reader.Transformer.Transform(
		ctx, aggTrace, entry.RootObject, displayMode,
		extensions,
		entry.StartTime, entry.EndTime,
	); err != nil {
		return nil, fmt.Errorf("trace transformation failed: %w", err)
	}

	reader.Logger.WithField("numTransformedSpans", len(aggTrace.Spans)).Info("query trace tree")

	return aggTrace, nil
}

const (
	CacheIdHighMask     uint64 = 0xFF00000000E1E3E7
	CacheIdHighBitShift uint64 = 6 * 4
)

func generateCacheId(mode tfconfig.Id) model.TraceID {
	// Format:
	// Low = random number
	// High = Prefix + mode + Suffix

	return model.TraceID{
		Low:  rand.Uint64(),
		High: CacheIdHighMask | (uint64(mode) << CacheIdHighBitShift),
	}
}

func extractDisplayMode(cacheId model.TraceID) tfconfig.Id {
	displayMode := cacheId.High >> CacheIdHighBitShift
	return tfconfig.Id(uint32(displayMode))
}

func filterTimeRange(spans []*model.Span, startTime, endTime time.Time) []*model.Span {
	var retained []*model.Span

	for _, span := range spans {
		traceSource, exists := model.KeyValues(span.Tags).FindByKey(zconstants.TraceSource)
		if exists && traceSource.VStr == zconstants.TraceSourceObject {
			// pseudo span, timestamp has no meaning
			span.StartTime = startTime
			span.Duration = endTime.Sub(startTime)
			retained = append(retained, span)
		} else {
			// normal span, filter away if start time is out of bounds
			if startTime.Before(span.StartTime) && span.StartTime.Before(endTime) {
				retained = append(retained, span)
			}
		}
	}

	return retained
}
