// Copyright (C) 2020 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package profile

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/math/f64"
	"github.com/google/gapid/core/math/u64"
	"github.com/google/gapid/core/os/device"
	"github.com/google/gapid/gapis/service"
)

const (
	gpuTimeMetricId       int32 = 0
	gpuWallTimeMetricId   int32 = 1
	counterMetricIdOffset int32 = 2
)

// For CPU commands, calculate their summarized GPU performance.
func ComputeCounters(ctx context.Context, slices *service.ProfilingData_GpuSlices, counters []*service.ProfilingData_Counter) (*service.ProfilingData_GpuCounters, error) {
	metrics := []*service.ProfilingData_GpuCounters_Metric{}

	// Filter out the slices that are at depth 0 and belong to a command,
	// then sort them based on the start time.
	groupToEntry := map[int32]*service.ProfilingData_GpuCounters_Entry{}
	for _, group := range slices.Groups {
		groupToEntry[group.Id] = &service.ProfilingData_GpuCounters_Entry{
			CommandIndex:  group.Link.Indices,
			MetricToValue: map[int32]*service.ProfilingData_GpuCounters_Perf{},
		}
	}
	filteredSlices := []*service.ProfilingData_GpuSlices_Slice{}
	for i := 0; i < len(slices.Slices); i++ {
		if slices.Slices[i].Depth == 0 && groupToEntry[slices.Slices[i].GroupId] != nil {
			filteredSlices = append(filteredSlices, slices.Slices[i])
		}
	}
	sort.Slice(filteredSlices, func(i, j int) bool {
		return filteredSlices[i].Ts < filteredSlices[j].Ts
	})

	// Group slices based on their group id.
	groupToSlices := map[int32][]*service.ProfilingData_GpuSlices_Slice{}
	for i := 0; i < len(filteredSlices); i++ {
		groupId := filteredSlices[i].GroupId
		groupToSlices[groupId] = append(groupToSlices[groupId], filteredSlices[i])
	}

	// Calculate GPU Time Performance and GPU Wall Time Performance for all leaf groups/commands.
	setTimeMetrics(groupToSlices, &metrics, groupToEntry)

	// Calculate GPU Counter Performances for all leaf groups/commands.
	setGpuCounterMetrics(ctx, groupToSlices, counters, filteredSlices, &metrics, groupToEntry)

	// Merge and organize the leaf entries.
	entries := mergeLeafEntries(ctx, metrics, groupToEntry)

	return &service.ProfilingData_GpuCounters{
		Metrics: metrics,
		Entries: entries,
	}, nil
}

// Create GPU time metric metadata, calculate time performance for each GPU
// slice group, and append the result to corresponding entries.
func setTimeMetrics(groupToSlices map[int32][]*service.ProfilingData_GpuSlices_Slice, metrics *[]*service.ProfilingData_GpuCounters_Metric, groupToEntry map[int32]*service.ProfilingData_GpuCounters_Entry) {
	*metrics = append(*metrics, &service.ProfilingData_GpuCounters_Metric{
		Id:   gpuTimeMetricId,
		Name: "GPU Time",
		Unit: strconv.Itoa(int(device.GpuCounterDescriptor_NANOSECOND)),
		Op:   service.ProfilingData_GpuCounters_Metric_Summation,
	})
	*metrics = append(*metrics, &service.ProfilingData_GpuCounters_Metric{
		Id:   gpuWallTimeMetricId,
		Name: "GPU Wall Time",
		Unit: strconv.Itoa(int(device.GpuCounterDescriptor_NANOSECOND)),
		Op:   service.ProfilingData_GpuCounters_Metric_Summation,
	})
	for groupId, slices := range groupToSlices {
		gpuTime, wallTime := gpuTimeForGroup(slices)
		entry := groupToEntry[groupId]
		entry.MetricToValue[gpuTimeMetricId] = &service.ProfilingData_GpuCounters_Perf{
			Estimate: float64(gpuTime),
			Min:      float64(gpuTime),
			Max:      float64(gpuTime),
		}
		entry.MetricToValue[gpuWallTimeMetricId] = &service.ProfilingData_GpuCounters_Perf{
			Estimate: float64(wallTime),
			Min:      float64(wallTime),
			Max:      float64(wallTime),
		}
	}
}

// Calculate GPU-time and wall-time for a specific GPU slice group.
func gpuTimeForGroup(slices []*service.ProfilingData_GpuSlices_Slice) (uint64, uint64) {
	gpuTime, wallTime := uint64(0), uint64(0)
	lastEnd := uint64(0)
	for _, slice := range slices {
		duration := slice.Dur
		gpuTime += duration
		if slice.Ts < lastEnd {
			if slice.Ts+slice.Dur <= lastEnd {
				continue // completely contained within the other, can ignore it.
			}
			duration -= lastEnd - slice.Ts
		}
		wallTime += duration
		lastEnd = slice.Ts + slice.Dur
	}
	return gpuTime, wallTime
}

// Create GPU counter metric metadata, calculate counter performance for each
// GPU slice group, and append the result to corresponding entries.
func setGpuCounterMetrics(ctx context.Context, groupToSlices map[int32][]*service.ProfilingData_GpuSlices_Slice, counters []*service.ProfilingData_Counter, globalSlices []*service.ProfilingData_GpuSlices_Slice, metrics *[]*service.ProfilingData_GpuCounters_Metric, groupToEntry map[int32]*service.ProfilingData_GpuCounters_Entry) {
	for i, counter := range counters {
		metricId := counterMetricIdOffset + int32(i)
		op := getCounterAggregationMethod(counter)
		*metrics = append(*metrics, &service.ProfilingData_GpuCounters_Metric{
			Id:   metricId,
			Name: counter.Name,
			Unit: counter.Unit,
			Op:   op,
		})
		if op != service.ProfilingData_GpuCounters_Metric_TimeWeightedAvg {
			log.E(ctx, "Counter aggregation method not implemented yet. Operation: %v", op)
			continue
		}
		concurrentSlicesCount := scanConcurrency(globalSlices, counter)
		for groupId, slices := range groupToSlices {
			estimateSet, minSet, maxSet := mapCounterSamples(slices, counter, concurrentSlicesCount)
			estimate := aggregateCounterSamples(estimateSet, counter)
			// Extra comparison here because minSet/maxSet only denote minimal/maximal
			// number of counter samples inclusion strategy, the aggregation result
			// may not be the smallest/largest actually.
			min, max := estimate, estimate
			if minSetRes := aggregateCounterSamples(minSet, counter); minSetRes != -1 {
				min = f64.MinOf(min, minSetRes)
				max = f64.MaxOf(max, minSetRes)
			}
			if maxSetRes := aggregateCounterSamples(maxSet, counter); maxSetRes != -1 {
				min = f64.MinOf(min, maxSetRes)
				max = f64.MaxOf(max, maxSetRes)
			}
			groupToEntry[groupId].MetricToValue[metricId] = &service.ProfilingData_GpuCounters_Perf{
				Estimate: estimate,
				Min:      min,
				Max:      max,
			}
		}
	}
}

// Scan global slices and count concurrent slices for each counter sample.
func scanConcurrency(globalSlices []*service.ProfilingData_GpuSlices_Slice, counter *service.ProfilingData_Counter) []int {
	slicesCount := make([]int, len(counter.Timestamps))
	for _, slice := range globalSlices {
		sStart, sEnd := slice.Ts, slice.Ts+slice.Dur
		for i := 1; i < len(counter.Timestamps); i++ {
			cStart, cEnd := counter.Timestamps[i-1], counter.Timestamps[i]
			if cEnd < sStart { // Sample earlier than GPU slice's span.
				continue
			} else if cStart > sEnd { // Sample later than GPU slice's span.
				break
			} else { // Sample overlaps with GPU slice's span.
				slicesCount[i]++
			}
		}
	}
	return slicesCount
}

// Map counter samples to GPU slice. When collecting samples, three sets will
// be maintained based on attribution strategy: the minimum set,
// the best guess set, and the maximum set.
// The returned results map {sample index} to {sample weight}.
func mapCounterSamples(slices []*service.ProfilingData_GpuSlices_Slice, counter *service.ProfilingData_Counter, concurrentSlicesCount []int) (map[int]float64, map[int]float64, map[int]float64) {
	estimateSet, minSet, maxSet := map[int]float64{}, map[int]float64{}, map[int]float64{}
	for _, slice := range slices {
		sStart, sEnd := slice.Ts, slice.Ts+slice.Dur
		for i := 1; i < len(counter.Timestamps); i++ {
			cStart, cEnd := counter.Timestamps[i-1], counter.Timestamps[i]
			concurrencyWeight := 1.0
			if concurrentSlicesCount[i] > 1 {
				concurrencyWeight = 1 / float64(concurrentSlicesCount[i])
			}
			if cEnd < sStart { // Sample earlier than GPU slice's span.
				continue
			} else if cStart > sEnd { // Sample later than GPU slice's span.
				break
			} else if cStart > sStart && cEnd < sEnd { // Sample is contained inside GPU slice's span.
				estimateSet[i] = 1 * concurrencyWeight
				// Only add to minSet when there's no concurrent slices, because of the
				// possibility that the sample belongs entirely to one of the slices.
				if concurrencyWeight == 1.0 {
					minSet[i] = 1
				}
				maxSet[i] = 1
			} else { // Sample contains, or partially overlap with GPU slice's span.
				percent := float64(0)
				if cEnd != cStart {
					percent = float64(u64.Min(cEnd, sEnd)-u64.Max(cStart, sStart)) / float64(cEnd-cStart) // Time overlap weight.
					percent *= concurrencyWeight
				}
				if _, ok := estimateSet[i]; !ok {
					estimateSet[i] = 0
				}
				estimateSet[i] += percent
				maxSet[i] = 1
			}
		}
	}
	return estimateSet, minSet, maxSet
}

// Aggregate counter samples to a single value based on counter weight.
func aggregateCounterSamples(sampleWeight map[int]float64, counter *service.ProfilingData_Counter) float64 {
	switch getCounterAggregationMethod(counter) {
	case service.ProfilingData_GpuCounters_Metric_Summation:
		ValueSum := float64(0)
		for idx, weight := range sampleWeight {
			ValueSum += counter.Values[idx] * weight
		}
		return ValueSum
	case service.ProfilingData_GpuCounters_Metric_TimeWeightedAvg:
		ValueSum, timeSum := float64(0), float64(0)
		for idx, weight := range sampleWeight {
			ValueSum += counter.Values[idx] * float64(counter.Timestamps[idx]-counter.Timestamps[idx-1]) * weight
			timeSum += float64(counter.Timestamps[idx]-counter.Timestamps[idx-1]) * weight
		}
		if timeSum != 0 {
			return ValueSum / timeSum
		} else {
			return -1
		}
	default:
		return -1
	}
}

// Merge leaf group entries if they belong to the same command, and also derive
// the parent command nodes' GPU performances based on the leaf entries.
func mergeLeafEntries(ctx context.Context, metrics []*service.ProfilingData_GpuCounters_Metric, groupToEntry map[int32]*service.ProfilingData_GpuCounters_Entry) []*service.ProfilingData_GpuCounters_Entry {
	mergedEntries := []*service.ProfilingData_GpuCounters_Entry{}

	// Find out all the self/parent command nodes that may need performance merging.
	indexToGroups := map[string][]int32{} // string formatted command index -> a list of contained groups referenced by group id.
	for groupId, entry := range groupToEntry {
		// The performance of one leaf group/command contributes to itself and all the ancestors up to the root command node.
		leafIdx := entry.CommandIndex
		for end := len(leafIdx); end > 0; end-- {
			mergedIdxStr := encodeIndex(leafIdx[0:end])
			indexToGroups[mergedIdxStr] = append(indexToGroups[mergedIdxStr], groupId)
		}
	}

	for commandIndex, leafGroupIds := range indexToGroups {
		mergedEntry := &service.ProfilingData_GpuCounters_Entry{
			CommandIndex:  decodeIndex(commandIndex),
			MetricToValue: map[int32]*service.ProfilingData_GpuCounters_Perf{},
		}
		for _, metric := range metrics {
			estimate, min, max := float64(-1), float64(-1), float64(-1)
			switch op := metric.Op; op {
			case service.ProfilingData_GpuCounters_Metric_Summation:
				estimate, min, max = float64(0), float64(0), float64(0)
				for _, id := range leafGroupIds {
					entry := groupToEntry[id]
					estimate += entry.MetricToValue[metric.Id].Estimate
					min += entry.MetricToValue[metric.Id].Min
					max += entry.MetricToValue[metric.Id].Max
				}
			case service.ProfilingData_GpuCounters_Metric_TimeWeightedAvg:
				timeSum, estimateValueSum, minValueSum, maxValueSum := float64(0), float64(0), float64(0), float64(0)
				for _, id := range leafGroupIds {
					entry := groupToEntry[id]
					gpuTime := entry.MetricToValue[gpuTimeMetricId].Estimate
					timeSum += gpuTime
					estimateValueSum += gpuTime * entry.MetricToValue[metric.Id].Estimate
					minValueSum += gpuTime * entry.MetricToValue[metric.Id].Min
					maxValueSum += gpuTime * entry.MetricToValue[metric.Id].Max
				}
				if timeSum != 0 {
					estimate, min, max = estimateValueSum/timeSum, minValueSum/timeSum, maxValueSum/timeSum
				}
			default:
				log.E(ctx, "Counter aggregation method not implemented yet. Operation: %v", op)
			}
			mergedEntry.MetricToValue[metric.Id] = &service.ProfilingData_GpuCounters_Perf{
				Estimate: estimate,
				Min:      min,
				Max:      max,
			}
		}
		mergedEntries = append(mergedEntries, mergedEntry)
	}

	return mergedEntries
}

// Evaluate and return the appropriate aggregation method for a GPU counter.
func getCounterAggregationMethod(counter *service.ProfilingData_Counter) service.ProfilingData_GpuCounters_Metric_AggregationOperator {
	// TODO: Use time-weighted average to aggregate all counters for now. May need vendor's support. Bug tracked with b/158057709.
	return service.ProfilingData_GpuCounters_Metric_TimeWeightedAvg
}

// Encode a command index, transform from array format to string format.
func encodeIndex(array_index []uint64) string {
	str := make([]string, len(array_index))
	for i, v := range array_index {
		str[i] = strconv.FormatUint(v, 10)
	}
	return strings.Join(str, ",")
}

// Decode a command index, transform from string format to array format.
func decodeIndex(str_index string) []uint64 {
	indexes := strings.Split(str_index, ",")
	array := make([]uint64, len(indexes))
	for i := range array {
		array[i], _ = strconv.ParseUint(indexes[i], 10, 0)
	}
	return array
}
