// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package datadogreceiver

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/DataDog/agent-payload/v5/gogen"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

func TestHandleSketchPayload(t *testing.T) {
	tests := []struct {
		name                      string
		sketchPayload             gogen.SketchPayload
		expectedSketchesCount     int
		expectedDogsketchesCounts []int
	}{
		{
			name: "Test simple sketch payload with single sketch",
			sketchPayload: gogen.SketchPayload{
				Sketches: []gogen.SketchPayload_Sketch{
					{
						Metric:        "Test1",
						Host:          "Host1",
						Tags:          []string{"env:tag1", "version:tag2"},
						Distributions: []gogen.SketchPayload_Sketch_Distribution{},
						Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
							{
								Ts:  400,
								Cnt: 6,
								Min: 1,
								Max: 3,
								Avg: 2.3333,
								Sum: 14.0,
								K:   []int32{1338, 1383, 1409},
								N:   []uint32{1, 2, 3},
							},
						},
					},
				},
			},
			expectedSketchesCount:     1,
			expectedDogsketchesCounts: []int{1},
		},
		{
			name: "Test simple sketch payload with multiple dogsketches",
			sketchPayload: gogen.SketchPayload{
				Sketches: []gogen.SketchPayload_Sketch{
					{
						Metric:        "Test1",
						Host:          "Host1",
						Tags:          []string{"env:tag1", "version:tag2"},
						Distributions: []gogen.SketchPayload_Sketch_Distribution{},
						Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
							{
								Ts:  400,
								Cnt: 6,
								Min: 1,
								Max: 3,
								Avg: 2.3333,
								Sum: 14.0,
								K:   []int32{1338, 1383, 1409},
								N:   []uint32{1, 2, 3},
							},
							{
								Ts:  500,
								Cnt: 15,
								Min: 4,
								Max: 5,
								Avg: 4.7333,
								Sum: 71.0,
								K:   []int32{1427, 1442, 1454},
								N:   []uint32{4, 5, 6},
							},
						},
					},
				},
			},
			expectedSketchesCount:     1,
			expectedDogsketchesCounts: []int{2},
		},
		{
			name: "Test sketch payload with multiple sketches",
			sketchPayload: gogen.SketchPayload{
				Sketches: []gogen.SketchPayload_Sketch{
					{
						Metric:        "Test1",
						Host:          "Host1",
						Tags:          []string{"env:tag1", "version:tag2"},
						Distributions: []gogen.SketchPayload_Sketch_Distribution{},
						Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
							{
								Ts:  400,
								Cnt: 6,
								Min: 1,
								Max: 3,
								Avg: 2.3333,
								Sum: 14.0,
								K:   []int32{1338, 1383, 1409},
								N:   []uint32{1, 2, 3},
							},
						},
					},
					{
						Metric:        "Test2",
						Host:          "Host1",
						Tags:          []string{"env:tag1", "version:tag2"},
						Distributions: []gogen.SketchPayload_Sketch_Distribution{},
						Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
							{
								Ts:  400,
								Cnt: 6,
								Min: 1,
								Max: 3,
								Avg: 2.3333,
								Sum: 14.0,
								K:   []int32{1338, 1383, 1409},
								N:   []uint32{1, 2, 3},
							},
						},
					},
				},
			},
			expectedSketchesCount:     2,
			expectedDogsketchesCounts: []int{1, 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pb, err := tt.sketchPayload.Marshal()
			assert.NoError(t, err)

			req, err := http.NewRequest(http.MethodPost, "/api/beta/sketches", io.NopCloser(bytes.NewReader(pb)))
			assert.Nil(t, err)
			metrics, err := handleSketchesPayload(req)
			assert.Nil(t, err)
			assert.Equal(t, tt.expectedSketchesCount, len(metrics))
			for i, metric := range metrics {
				assert.Equal(t, tt.expectedDogsketchesCounts[i], len(metric.Dogsketches))
			}
		})
	}
}

func TestTranslateSketches(t *testing.T) {
	tests := []struct {
		name     string
		sketches []gogen.SketchPayload_Sketch
	}{
		{
			name: "Single sketch with only positive buckets and no zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
					},
				},
			},
		},
		{
			name: "Single sketch with only negative buckets and no zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 941,
							Min: -6.0,
							Max: -1.0,
							Avg: -3.0,
							Sum: 2038.0,
							K:   []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338},
							N:   []uint32{152, 231, 97, 55, 101, 239, 66},
						},
					},
				},
			},
		},
		{
			name: "Single sketch with negative and positive buckets and no zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 1952,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 1019.0,
							K:   []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{152, 231, 97, 55, 101, 239, 66, 43, 99, 123, 62, 194, 251, 239},
						},
					},
				},
			},
		},
		{
			name: "Single sketch with only positive buckets and zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 954,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2049.0,
							K:   []int32{0, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 231, 97, 55, 101, 239, 66},
						},
					},
				},
			},
		},
		{
			name: "Single sketch with only negative buckets and no zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 941,
							Min: -6.0,
							Max: -1.0,
							Avg: -3.0,
							Sum: -2049,
							K:   []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338},
							N:   []uint32{152, 231, 97, 55, 101, 239, 66},
						},
					},
				},
			},
		},
		{
			name: "Single sketch with negative and positive buckets and zero bucket",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  400,
							Cnt: 1964,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 1589.0,
							K:   []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338, 0, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{152, 231, 97, 55, 101, 239, 66, 12, 43, 99, 123, 62, 194, 251, 239},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := newMetricsTranslator(zap.NewNop())
			mt.buildInfo = component.BuildInfo{
				Command:     "otelcol",
				Description: "OpenTelemetry Collector",
				Version:     "latest",
			}
			result := translateSketches(tt.sketches, mt)
			assert.Equal(t, 1, result.MetricCount())
			assert.Equal(t, 1, result.DataPointCount())
			metrics := result.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
			assert.Equal(t, 1, result.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().Len())

			metric := metrics.At(0)
			assert.Equal(t, pmetric.MetricTypeExponentialHistogram, metric.Type())

			for _, sketch := range tt.sketches {
				assert.Equal(t, sketch.GetMetric(), metric.Name())
				for i, dogsketch := range sketch.Dogsketches {
					m := metric.ExponentialHistogram().DataPoints().At(i)
					assert.Equal(t, pcommon.Timestamp(dogsketch.Ts*1_000_000_000), m.Timestamp())
					assert.Equal(t, uint64(dogsketch.Cnt), m.Count())
					assert.Equal(t, dogsketch.Sum, m.Sum())
					assert.Equal(t, dogsketch.Min, m.Min())
					assert.Equal(t, dogsketch.Max, m.Max())
					assert.Equal(t, m.Count(), totalHistBucketCounts(m)) // Ensure that buckets contain same number of counts as total count
				}
			}
		})
	}
}

func TestSketchTemporality(t *testing.T) {
	tests := []struct {
		name     string
		sketches []gogen.SketchPayload_Sketch
	}{
		{
			name: "Two metrics with multiple data points",
			sketches: []gogen.SketchPayload_Sketch{
				{
					Metric:        "Test1",
					Host:          "Host1",
					Tags:          []string{"version:tag1"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  100,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
						{
							Ts:  200,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
						{
							Ts:  300,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
					},
				},
				{
					Metric:        "Test2",
					Host:          "Host2",
					Tags:          []string{"env:tag1", "version:tag2"},
					Distributions: []gogen.SketchPayload_Sketch_Distribution{},
					Dogsketches: []gogen.SketchPayload_Sketch_Dogsketch{
						{
							Ts:  20,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
						{
							Ts:  30,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
						{
							Ts:  40,
							Cnt: 1029,
							Min: 1.0,
							Max: 6.0,
							Avg: 3.0,
							Sum: 2038.0,
							K:   []int32{0, 1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
							N:   []uint32{13, 152, 75, 231, 97, 55, 101, 239, 66},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := newMetricsTranslator(zap.NewNop())
			mt.buildInfo = component.BuildInfo{
				Command:     "otelcol",
				Description: "OpenTelemetry Collector",
				Version:     "latest",
			}
			result := translateSketches(tt.sketches, mt)
			assert.Equal(t, 2, result.MetricCount())
			assert.Equal(t, 6, result.DataPointCount())

			assert.Equal(t, "otelcol/datadogreceiver", result.ResourceMetrics().At(0).ScopeMetrics().At(0).Scope().Name())
			assert.Equal(t, component.NewDefaultBuildInfo().Version, result.ResourceMetrics().At(0).ScopeMetrics().At(0).Scope().Version())

			metrics := result.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
			assert.Equal(t, 2, result.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().Len())

			metric1 := metrics.At(0)
			assert.Equal(t, pmetric.MetricTypeExponentialHistogram, metric1.Type())
			metric2 := metrics.At(1)
			assert.Equal(t, pmetric.MetricTypeExponentialHistogram, metric2.Type())

			var lastTimestamp pcommon.Timestamp
			for i := 0; i < metric1.ExponentialHistogram().DataPoints().Len(); i++ {
				m := metric1.ExponentialHistogram().DataPoints().At(i)
				if i == 0 {
					assert.Equal(t, m.StartTimestamp(), pcommon.Timestamp(0))
				} else {
					assert.Equal(t, m.StartTimestamp(), lastTimestamp)
				}
				lastTimestamp = m.Timestamp()
			}
			for i := 0; i < metric2.ExponentialHistogram().DataPoints().Len(); i++ {
				m := metric2.ExponentialHistogram().DataPoints().At(i)
				if i == 0 {
					assert.Equal(t, m.StartTimestamp(), pcommon.Timestamp(0))
				} else {
					assert.Equal(t, m.StartTimestamp(), lastTimestamp)
				}
				lastTimestamp = m.Timestamp()
			}
		})
	}
}

func TestConvertBucketLayout(t *testing.T) {
	tests := []struct {
		name                    string
		inputBuckets            map[int]uint64
		expectedOffset          int32
		expectedBucketCountsLen int
		expectedBucketCounts    map[int]uint64
	}{
		{
			name:                    "Empty input buckets",
			inputBuckets:            map[int]uint64{},
			expectedOffset:          0,
			expectedBucketCountsLen: 0,
			expectedBucketCounts:    map[int]uint64{},
		},
		{
			name:                    "Non-empty input buckets and no offset",
			inputBuckets:            map[int]uint64{5: 75, 64: 33, 83: 239, 0: 152, 32: 231, 50: 24, 51: 73, 63: 22, 74: 79, 75: 22, 90: 66},
			expectedOffset:          0,
			expectedBucketCountsLen: 91,
			expectedBucketCounts:    map[int]uint64{0: 152, 5: 75, 32: 231, 50: 24, 51: 73, 63: 22, 64: 33, 74: 79, 75: 22, 83: 239, 90: 66},
		},
		{
			name:                    "Non-empty input buckets with offset",
			inputBuckets:            map[int]uint64{5: 75, 64: 33, 83: 239, 32: 231, 50: 24, 51: 73, 63: 22, 74: 79, 75: 22, 90: 66},
			expectedOffset:          5,
			expectedBucketCountsLen: 86,
			expectedBucketCounts:    map[int]uint64{0: 75, 27: 231, 45: 24, 46: 73, 58: 22, 59: 33, 69: 79, 70: 22, 78: 239, 85: 66},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputBuckets := pmetric.NewExponentialHistogramDataPointBuckets()

			convertBucketLayout(tt.inputBuckets, outputBuckets)

			assert.Equal(t, tt.expectedOffset, outputBuckets.Offset())
			assert.Equal(t, tt.expectedBucketCountsLen, outputBuckets.BucketCounts().Len())

			for k, v := range outputBuckets.BucketCounts().AsRaw() {
				assert.Equal(t, tt.expectedBucketCounts[k], v)
			}
		})
	}
}

func TestMapSketchBucketsToHistogramBuckets(t *testing.T) {
	tests := []struct {
		name                    string
		sketchKeys              []int32
		sketchCounts            []uint32
		expectedNegativeBuckets map[int]uint64
		expectedPositiveBuckets map[int]uint64
		expectedZeroCount       uint64
	}{
		{
			name:                    "Empty sketch buckets",
			sketchKeys:              []int32{},
			sketchCounts:            []uint32{},
			expectedNegativeBuckets: map[int]uint64{},
			expectedPositiveBuckets: map[int]uint64{},
			expectedZeroCount:       0,
		},
		{
			name:                    "Only positive buckets and no zero bucket",
			sketchKeys:              []int32{1338, 1345, 1383, 1409, 1427, 1442, 1454, 1464},
			sketchCounts:            []uint32{152, 75, 231, 97, 55, 101, 239, 66},
			expectedNegativeBuckets: map[int]uint64{},
			expectedPositiveBuckets: map[int]uint64{0: 152, 5: 75, 32: 231, 50: 24, 51: 73, 63: 22, 64: 33, 74: 79, 75: 22, 83: 239, 90: 66},
			expectedZeroCount:       0,
		},
		{
			name:                    "Only negative buckets and no zero bucket",
			sketchKeys:              []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338},
			sketchCounts:            []uint32{152, 231, 97, 55, 101, 239, 66},
			expectedNegativeBuckets: map[int]uint64{0: 66, 32: 239, 50: 25, 51: 76, 63: 22, 64: 33, 74: 76, 75: 21, 83: 231, 90: 152},
			expectedPositiveBuckets: map[int]uint64{},
			expectedZeroCount:       0,
		},
		{
			name:                    "Negative and positive buckets and no zero bucket",
			sketchKeys:              []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
			sketchCounts:            []uint32{152, 231, 97, 55, 101, 239, 66, 43, 99, 123, 62, 194, 251, 239},
			expectedNegativeBuckets: map[int]uint64{0: 66, 32: 239, 50: 25, 51: 76, 63: 22, 64: 33, 74: 76, 75: 21, 83: 231, 90: 152},
			expectedPositiveBuckets: map[int]uint64{0: 43, 32: 99, 50: 30, 51: 93, 63: 25, 64: 37, 74: 152, 75: 42, 83: 251, 90: 239},
			expectedZeroCount:       0,
		},
		{
			name:                    "Only positive buckets and zero bucket",
			sketchKeys:              []int32{0, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
			sketchCounts:            []uint32{13, 152, 231, 97, 55, 101, 239, 66},
			expectedNegativeBuckets: map[int]uint64{},
			expectedPositiveBuckets: map[int]uint64{0: 152, 32: 231, 50: 24, 51: 73, 63: 22, 64: 33, 74: 79, 75: 22, 83: 239, 90: 66},
			expectedZeroCount:       13,
		},
		{
			name:                    "Only negative buckets and zero bucket",
			sketchKeys:              []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338, 0},
			sketchCounts:            []uint32{152, 231, 97, 55, 101, 239, 66, 13},
			expectedNegativeBuckets: map[int]uint64{0: 66, 32: 239, 50: 25, 51: 76, 63: 22, 64: 33, 74: 76, 75: 21, 83: 231, 90: 152},
			expectedPositiveBuckets: map[int]uint64{},
			expectedZeroCount:       13,
		},
		{
			name:                    "Negative and positive buckets and zero bucket",
			sketchKeys:              []int32{-1464, -1454, -1442, -1427, -1409, -1383, -1338, 0, 1338, 1383, 1409, 1427, 1442, 1454, 1464},
			sketchCounts:            []uint32{152, 231, 97, 55, 101, 239, 66, 12, 43, 99, 123, 62, 194, 251, 239},
			expectedNegativeBuckets: map[int]uint64{0: 66, 32: 239, 50: 25, 51: 76, 63: 22, 64: 33, 74: 76, 75: 21, 83: 231, 90: 152},
			expectedPositiveBuckets: map[int]uint64{0: 43, 32: 99, 50: 30, 51: 93, 63: 25, 64: 37, 74: 152, 75: 42, 83: 251, 90: 239},
			expectedZeroCount:       12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			negativeBuckets, positiveBuckets, zeroCount := mapSketchBucketsToHistogramBuckets(tt.sketchKeys, tt.sketchCounts)

			assert.Equal(t, tt.expectedNegativeBuckets, negativeBuckets)
			assert.Equal(t, tt.expectedPositiveBuckets, positiveBuckets)
			assert.Equal(t, tt.expectedZeroCount, zeroCount)
		})
	}
}
