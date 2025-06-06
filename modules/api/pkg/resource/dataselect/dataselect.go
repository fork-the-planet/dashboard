// Copyright 2017 The Kubernetes Authors.
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

package dataselect

import (
	"sort"

	"k8s.io/klog/v2"

	"k8s.io/dashboard/api/pkg/args"
	metricapi "k8s.io/dashboard/api/pkg/integration/metric/api"
	"k8s.io/dashboard/errors"
)

// GenericDataCell describes the interface of the data cell that contains all the necessary methods needed to perform
// complex data selection
// GenericDataSelect takes a list of these interfaces and performs selection operation.
// Therefore as long as the list is composed of GenericDataCells you can perform any data selection!
type DataCell interface {
	// GetPropertyAtIndex returns the property of this data cell.
	// Value returned has to have Compare method which is required by Sort functionality of DataSelect.
	GetProperty(PropertyName) ComparableValue
}

// MetricDataCell extends interface of DataCells and additionally supports metric download.
type MetricDataCell interface {
	DataCell
	// GetResourceSelector returns ResourceSelector for this resource. The ResourceSelector can be used to get,
	// HeapsterSelector which in turn can be used to download metrics.
	GetResourceSelector() *metricapi.ResourceSelector
}

// ComparableValue hold any value that can be compared to its own kind.
type ComparableValue interface {
	// Compares in with other value. Returns 1 if other value is smaller, 0 if they are the same, -1 if other is larger.
	Compare(ComparableValue) int
	// Returns true if in value contains or is equal to other value, false otherwise.
	Contains(ComparableValue) bool
}

// SelectableData contains all the required data to perform data selection.
// It implements sort.Interface so its sortable under sort.Sort
// You can use its Select method to get selected GenericDataCell list.
type DataSelector struct {
	// GenericDataList hold generic data cells that are being selected.
	GenericDataList []DataCell
	// DataSelectQuery holds instructions for data select.
	DataSelectQuery *DataSelectQuery
	// CachedResources stores resources that may be needed during data selection process
	CachedResources *metricapi.CachedResources
	// CumulativeMetricsPromises is a list of promises holding aggregated metrics for resources in GenericDataList.
	// The metrics will be calculated after calling GetCumulativeMetrics method.
	CumulativeMetricsPromises metricapi.MetricPromises
	// MetricsPromises is a list of promises holding metrics for resources in GenericDataList.
	// The metrics will be calculated after calling GetMetrics method. Metric will not be
	// aggregated and can are used to display sparklines on pod list.
	MetricsPromises metricapi.MetricPromises
}

// Implementation of sort.Interface so that we can use built-in sort function (sort.Sort) for sorting SelectableData

// Len returns the length of data inside SelectableData.
func (in DataSelector) Len() int { return len(in.GenericDataList) }

// Swap swaps 2 indices inside SelectableData.
func (in DataSelector) Swap(i, j int) {
	in.GenericDataList[i], in.GenericDataList[j] = in.GenericDataList[j], in.GenericDataList[i]
}

// Less compares 2 indices inside SelectableData and returns true if first index is larger.
func (in DataSelector) Less(i, j int) bool {
	for _, sortBy := range in.DataSelectQuery.SortQuery.SortByList {
		a := in.GenericDataList[i].GetProperty(sortBy.Property)
		b := in.GenericDataList[j].GetProperty(sortBy.Property)
		// ignore sort completely if property name not found
		if a == nil || b == nil {
			break
		}
		cmp := a.Compare(b)
		if cmp == 0 { // values are the same. Just continue to next sortBy
			continue
		} else { // values different
			return (cmp == -1 && sortBy.Ascending) || (cmp == 1 && !sortBy.Ascending)
		}
	}
	return false
}

// Sort sorts the data inside as instructed by DataSelectQuery and returns itin to allow method chaining.
func (in *DataSelector) Sort() *DataSelector {
	sort.Sort(*in)
	return in
}

// Filter the data inside as instructed by DataSelectQuery and returns itin to allow method chaining.
func (in *DataSelector) Filter() *DataSelector {
	filteredList := []DataCell{}

	for _, c := range in.GenericDataList {
		matches := true
		for _, filterBy := range in.DataSelectQuery.FilterQuery.FilterByList {
			v := c.GetProperty(filterBy.Property)
			if v == nil || !v.Contains(filterBy.Value) {
				matches = false
				break
			}
		}
		if matches {
			filteredList = append(filteredList, c)
		}
	}

	in.GenericDataList = filteredList
	return in
}

func (in *DataSelector) getMetrics(metricClient metricapi.MetricClient) (
	[]metricapi.MetricPromises, error) {
	metricPromises := make([]metricapi.MetricPromises, 0)

	if metricClient == nil {
		return metricPromises, nil
	}

	metricNames := in.DataSelectQuery.MetricQuery.MetricNames
	if metricNames == nil {
		return metricPromises, errors.NewInternal("No metrics specified. Skipping metrics.")
	}

	selectors := make([]metricapi.ResourceSelector, len(in.GenericDataList))
	for i, dataCell := range in.GenericDataList {
		// make sure data cells support metrics
		metricDataCell, ok := dataCell.(MetricDataCell)
		if !ok {
			klog.V(0).InfoS("Data cell does not implement MetricDataCell. Skipping.", "dataCell", dataCell)
			continue
		}

		selectors[i] = *metricDataCell.GetResourceSelector()
	}

	for _, metricName := range metricNames {
		promises := metricClient.DownloadMetric(selectors, metricName, in.CachedResources)
		metricPromises = append(metricPromises, promises)
	}

	return metricPromises, nil
}

// GetMetrics downloads metrics for data cells currently present in in.GenericDataList as instructed
// by MetricQuery and inserts resulting MetricPromises to in.MetricsPromises.
func (in *DataSelector) GetMetrics(metricClient metricapi.MetricClient) *DataSelector {
	metricPromisesList, err := in.getMetrics(metricClient)
	if err != nil {
		klog.ErrorS(err, "error during getting metrics")
		return in
	}

	if len(metricPromisesList) == 0 {
		return in
	}

	metricPromises := make(metricapi.MetricPromises, 0)
	for _, promises := range metricPromisesList {
		metricPromises = append(metricPromises, promises...)
	}

	in.MetricsPromises = metricPromises
	return in
}

// GetCumulativeMetrics downloads and aggregates metrics for data cells currently present in in.GenericDataList as instructed
// by MetricQuery and inserts resulting MetricPromises to in.CumulativeMetricsPromises.
func (in *DataSelector) GetCumulativeMetrics(metricClient metricapi.MetricClient) *DataSelector {
	metricPromisesList, err := in.getMetrics(metricClient)
	if err != nil {
		klog.ErrorS(err, "error during getting metrics")
		return in
	}

	if len(metricPromisesList) == 0 {
		return in
	}

	metricNames := in.DataSelectQuery.MetricQuery.MetricNames
	if metricNames == nil {
		klog.V(args.LogLevelVerbose).Info("Metrics names not provided. Skipping.")
		return in
	}

	aggregations := in.DataSelectQuery.MetricQuery.Aggregations
	if aggregations == nil {
		aggregations = metricapi.OnlyDefaultAggregation
	}

	metricPromises := make(metricapi.MetricPromises, 0)
	for i, metricName := range metricNames {
		promises := metricClient.AggregateMetrics(metricPromisesList[i], metricName, aggregations)
		metricPromises = append(metricPromises, promises...)
	}

	in.CumulativeMetricsPromises = metricPromises
	return in
}

// Paginates the data inside as instructed by DataSelectQuery and returns itin to allow method chaining.
func (in *DataSelector) Paginate() *DataSelector {
	pQuery := in.DataSelectQuery.PaginationQuery
	dataList := in.GenericDataList
	startIndex, endIndex := pQuery.GetPaginationSettings(len(dataList))

	// Return all items if provided settings do not meet requirements
	if !pQuery.IsValidPagination() {
		return in
	}
	// Return no items if requested page does not exist
	if !pQuery.IsPageAvailable(len(in.GenericDataList), startIndex) {
		in.GenericDataList = []DataCell{}
		return in
	}

	in.GenericDataList = dataList[startIndex:endIndex]
	return in
}

// GenericDataSelect takes a list of GenericDataCells and DataSelectQuery and returns selected data as instructed by dsQuery.
func GenericDataSelect(dataList []DataCell, dsQuery *DataSelectQuery) []DataCell {
	SelectableData := DataSelector{
		GenericDataList: dataList,
		DataSelectQuery: dsQuery,
	}
	return SelectableData.Sort().Paginate().GenericDataList
}

// GenericDataSelectWithFilter takes a list of GenericDataCells and DataSelectQuery and returns selected data as instructed by dsQuery.
func GenericDataSelectWithFilter(dataList []DataCell, dsQuery *DataSelectQuery) ([]DataCell, int) {
	SelectableData := DataSelector{
		GenericDataList: dataList,
		DataSelectQuery: dsQuery,
	}
	// Pipeline is Filter -> Sort -> CollectMetrics -> Paginate
	filtered := SelectableData.Filter()
	filteredTotal := len(filtered.GenericDataList)
	processed := filtered.Sort().Paginate()
	return processed.GenericDataList, filteredTotal
}

// GenericDataSelect takes a list of GenericDataCells and DataSelectQuery and returns selected data as instructed by dsQuery.
func GenericDataSelectWithMetrics(dataList []DataCell, dsQuery *DataSelectQuery,
	cachedResources *metricapi.CachedResources, metricClient metricapi.MetricClient) (
	[]DataCell, metricapi.MetricPromises) {
	SelectableData := DataSelector{
		GenericDataList: dataList,
		DataSelectQuery: dsQuery,
		CachedResources: cachedResources,
	}
	// Pipeline is Filter -> Sort -> CollectMetrics -> Paginate
	processed := SelectableData.Sort().GetCumulativeMetrics(metricClient).Paginate()
	return processed.GenericDataList, processed.CumulativeMetricsPromises
}

// GenericDataSelect takes a list of GenericDataCells and DataSelectQuery and returns selected data as instructed by dsQuery.
func GenericDataSelectWithFilterAndMetrics(dataList []DataCell, dsQuery *DataSelectQuery,
	cachedResources *metricapi.CachedResources, metricClient metricapi.MetricClient) (
	[]DataCell, metricapi.MetricPromises, int) {
	SelectableData := DataSelector{
		GenericDataList: dataList,
		DataSelectQuery: dsQuery,
		CachedResources: cachedResources,
	}
	// Pipeline is Filter -> Sort -> CollectMetrics -> Paginate
	filtered := SelectableData.Filter()
	filteredTotal := len(filtered.GenericDataList)
	processed := filtered.Sort().GetCumulativeMetrics(metricClient).Paginate()
	return processed.GenericDataList, processed.CumulativeMetricsPromises, filteredTotal
}

// PodListMetrics returns metrics for every resource on the dataList without aggregating data.
func PodListMetrics(dataList []DataCell, dsQuery *DataSelectQuery,
	metricClient metricapi.MetricClient) metricapi.MetricPromises {
	selectableData := DataSelector{
		GenericDataList: dataList,
		DataSelectQuery: dsQuery,
		CachedResources: metricapi.NoResourceCache,
	}

	processed := selectableData.GetMetrics(metricClient)
	return processed.MetricsPromises
}
