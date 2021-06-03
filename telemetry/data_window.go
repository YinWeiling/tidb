// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/store/tikv/logutil"

	// "github.com/pingcap/tidb/types"
	"github.com/pingcap/errors"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	pmodel "github.com/prometheus/common/model"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

var (
	// CurrentExecuteCount is CurrentExecuteCount
	CurrentExecuteCount atomic.Uint64
	// CurrentTiFlashPushDownCount is CurrentTiFlashPushDownCount
	CurrentTiFlashPushDownCount atomic.Uint64
	// CurrentTiFlashExchangePushDownCount is CurrentTiFlashExchangePushDownCount
	CurrentTiFlashExchangePushDownCount atomic.Uint64
	// CurrentCoprCacheHitRatioGTE0Count is CurrentCoprCacheHitRatioGTE1Count
	CurrentCoprCacheHitRatioGTE0Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE1Count is CurrentCoprCacheHitRatioGTE1Count
	CurrentCoprCacheHitRatioGTE1Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE10Count is CurrentCoprCacheHitRatioGTE10Count
	CurrentCoprCacheHitRatioGTE10Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE20Count is CurrentCoprCacheHitRatioGTE20Count
	CurrentCoprCacheHitRatioGTE20Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE40Count is CurrentCoprCacheHitRatioGTE40Count
	CurrentCoprCacheHitRatioGTE40Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE80Count is CurrentCoprCacheHitRatioGTE80Count
	CurrentCoprCacheHitRatioGTE80Count atomic.Uint64
	// CurrentCoprCacheHitRatioGTE100Count is CurrentCoprCacheHitRatioGTE100Count
	CurrentCoprCacheHitRatioGTE100Count atomic.Uint64
)

const (
	// WindowSize determines how long some data is aggregated by.
	WindowSize = 3 * time.Minute
	// SubWindowSize determines how often data is rotated.
	SubWindowSize = 1 * time.Minute

	maxSubWindowLength         = int(ReportInterval / SubWindowSize) // TODO: Ceiling?
	maxSubWindowLengthInWindow = int(WindowSize / SubWindowSize)     // TODO: Ceiling?
	promReadTimeout            = time.Second * 10
)

type windowData struct {
	BeginAt        time.Time          `json:"beginAt"`
	ExecuteCount   uint64             `json:"executeCount"`
	TiFlashUsage   tiFlashUsageData   `json:"tiFlashUsage"`
	CoprCacheUsage coprCacheUsageData `json:"coprCacheUsage"`
	SQLUsage       sqlUsageData       `json:"SQLUsage"`
}

type sqlType struct {
	Use      int64 `json:"Use"`
	Show     int64 `json:"Show"`
	Begin    int64 `json:"Begin"`
	Commit   int64 `json:"Commit"`
	Rollback int64 `json:"Rollback"`
	Insert   int64 `json:"Insert"`
	Replace  int64 `json:"Replace"`
	Delete   int64 `json:"Delete"`
	Update   int64 `json:"Update"`
	Select   int64 `json:"Select"`
	Other    int64 `json:"Other"`
}

type sqlUsageData struct {
	SQLTotal         int64     `json:"total"`
	SQLType          sqlType   `json:"type"`
	SQLDurationTotal SQLBucket `json:"durationTotal"`
}

type coprCacheUsageData struct {
	GTE0   uint64 `json:"gte0"`
	GTE1   uint64 `json:"gte1"`
	GTE10  uint64 `json:"gte10"`
	GTE20  uint64 `json:"gte20"`
	GTE40  uint64 `json:"gte40"`
	GTE80  uint64 `json:"gte80"`
	GTE100 uint64 `json:"gte100"`
}

type tiFlashUsageData struct {
	PushDown         uint64 `json:"pushDown"`
	ExchangePushDown uint64 `json:"exchangePushDown"`
}

// type promResult struct {
// 	sqlResult pmodel.Value
// 	durResult pmodel.Value
// }

var (
	rotatedSubWindows []*windowData
	subWindowsLock    = sync.RWMutex{}
)

// SQL duration bucket
type SQLBucket map[string]int64

// const SQLBucketNum = 29 //prometheus.ExponentialBuckets(0.001, 2, 28), and 1 more +Inf

func getSQLSum(sqlTypeDeta *sqlType) int64 {
	result := sqlTypeDeta.Use
	result += sqlTypeDeta.Show
	result += sqlTypeDeta.Begin
	result += sqlTypeDeta.Commit
	result += sqlTypeDeta.Rollback
	result += sqlTypeDeta.Insert
	result += sqlTypeDeta.Replace
	result += sqlTypeDeta.Delete
	result += sqlTypeDeta.Update
	result += sqlTypeDeta.Select
	result += sqlTypeDeta.Other
	return result
}

func readSQLMetric(timepoint time.Time, SQLresult sqlUsageData) (sqlUsageData, error) {
	ctx := context.TODO()
	quantile := 0.99

	result, err := querySQLMetric(ctx, timepoint, quantile)
	if err != nil {
		if err1, ok := err.(*promv1.Error); ok {
			return SQLresult, errors.Errorf("query metric error, msg: %v, detail: %v", err1.Msg, err1.Detail)
		}
		return SQLresult, errors.Errorf("query metric error: %v", err.Error())
	}
	logutil.BgLogger().Info("perpare to analys")
	dataAnylis(result, &SQLresult)
	durResult, err := queryDurMetric(ctx, timepoint, quantile)
	if err != nil {
		if err1, ok := err.(*promv1.Error); ok {
			return SQLresult, errors.Errorf("query metric error, msg: %v, detail: %v", err1.Msg, err1.Detail)
		}
		return SQLresult, errors.Errorf("query metric error: %v", err.Error())
	}
	switch durResult.Type() {
	case pmodel.ValVector:
		promVec := durResult.(pmodel.Vector)
		for _, sample := range promVec {
			metric := sample.Metric
			bucketName := metric["le"] //hardcode bucket upper bound
			SQLresult.SQLDurationTotal[string(bucketName)] = int64(sample.Value)
		}
	}
	logutil.BgLogger().Info(fmt.Sprintf("bucket:%s", bucketMap2Json(SQLresult.SQLDurationTotal)))
	return SQLresult, nil

}

func querySQLMetric(ctx context.Context, queryTime time.Time, quantile float64) (result pmodel.Value, err error) {
	// Add retry to avoid network error.
	var prometheusAddr string
	for i := 0; i < 5; i++ {
		//TODO: the prometheus will be Integrated into the PD, then we need to query the prometheus in PD directly, which need change the quire API
		prometheusAddr, err = infosync.GetPrometheusAddr()
		if err == nil || err == infosync.ErrPrometheusAddrIsNotSet {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	promClient, err := api.NewClient(api.Config{
		Address: prometheusAddr,
	})
	if err != nil {
		return nil, err
	}
	promQLAPI := promv1.NewAPI(promClient)
	ctx, cancel := context.WithTimeout(ctx, promReadTimeout)
	defer cancel()
	promQL := "tidb_executor_statement_total{}"
	// Add retry to avoid network error.
	for i := 0; i < 5; i++ {
		result, _, err = promQLAPI.Query(ctx, promQL, queryTime)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return result, err
}

func queryDurMetric(ctx context.Context, queryTime time.Time, quantile float64) (result pmodel.Value, err error) {
	// Add retry to avoid network error.
	var prometheusAddr string
	for i := 0; i < 5; i++ {
		//TODO: the prometheus will be Integrated into the PD, then we need to query the prometheus in PD directly, which need change the quire API
		prometheusAddr, err = infosync.GetPrometheusAddr()
		if err == nil || err == infosync.ErrPrometheusAddrIsNotSet {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	promClient, err := api.NewClient(api.Config{
		Address: prometheusAddr,
	})
	if err != nil {
		return nil, err
	}
	promQLAPI := promv1.NewAPI(promClient)
	ctx, cancel := context.WithTimeout(ctx, promReadTimeout)
	defer cancel()
	promQL := "sum(tidb_server_handle_query_duration_seconds_bucket{sql_type!=\"internal\"}) by (le)"
	// Add retry to avoid network error.
	for i := 0; i < 5; i++ {
		result, _, err = promQLAPI.Query(ctx, promQL, queryTime)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return result, err
}

func dataAnylis(promResult pmodel.Value, SQLresult *sqlUsageData) {
	switch promResult.Type() {
	case pmodel.ValVector:
		matrix := promResult.(pmodel.Vector)
		for _, m := range matrix {
			v := m.Value
			genRecord(m.Metric, v, SQLresult)
		}
	}
}

func bucketMap2Json(paramMap SQLBucket) string {
	dataType, err := json.Marshal(paramMap)
	if err != nil {
		return ""
	}
	return string(dataType)
}

func calculateDeltaB(cur SQLBucket, last SQLBucket) SQLBucket {
	deltaMap := make(SQLBucket)
	for key, value := range cur {
		deltaMap[key] = value - (last)[key]
	}
	return deltaMap
}

func genRecord(metric pmodel.Metric, pair pmodel.SampleValue, SQLresult *sqlUsageData) {
	v := ""
	if metric != nil {
		v = string(metric[pmodel.LabelName("type")])
	}
	if len(v) == 0 {
		v = "total"
	}
	var record int64
	if math.IsNaN(float64(pair)) {
		record = 0
	} else {
		record = int64(float64(pair))
	}
	switch v {
	case "total":
		SQLresult.SQLTotal = record
	case "Use":
		SQLresult.SQLType.Use = record
	case "Show":
		SQLresult.SQLType.Show = record
	case "Begin":
		SQLresult.SQLType.Begin = record
	case "Commit":
		SQLresult.SQLType.Commit = record
	case "Rollback":
		SQLresult.SQLType.Rollback = record
	case "Insert":
		SQLresult.SQLType.Insert = record
	case "Replace":
		SQLresult.SQLType.Replace = record
	case "Delete":
		SQLresult.SQLType.Delete = record
	case "Update":
		SQLresult.SQLType.Update = record
	case "Select":
		SQLresult.SQLType.Select = record
	default:
		SQLresult.SQLType.Other = record
	}
}

// RotateSubWindow rotates the telemetry sub window.
// ctx sessionctx.Context
func RotateSubWindow() {
	thisSubWindow := windowData{
		BeginAt:      time.Now(),
		ExecuteCount: CurrentExecuteCount.Swap(0),
		TiFlashUsage: tiFlashUsageData{
			PushDown:         CurrentTiFlashPushDownCount.Swap(0),
			ExchangePushDown: CurrentTiFlashExchangePushDownCount.Swap(0),
		},
		CoprCacheUsage: coprCacheUsageData{
			GTE0:   CurrentCoprCacheHitRatioGTE0Count.Swap(0),
			GTE1:   CurrentCoprCacheHitRatioGTE1Count.Swap(0),
			GTE10:  CurrentCoprCacheHitRatioGTE10Count.Swap(0),
			GTE20:  CurrentCoprCacheHitRatioGTE20Count.Swap(0),
			GTE40:  CurrentCoprCacheHitRatioGTE40Count.Swap(0),
			GTE80:  CurrentCoprCacheHitRatioGTE80Count.Swap(0),
			GTE100: CurrentCoprCacheHitRatioGTE100Count.Swap(0),
		},
		SQLUsage: sqlUsageData{
			SQLTotal:         0,
			SQLDurationTotal: make(SQLBucket),
		},
	}
	var err error
	thisSubWindow.SQLUsage, err = readSQLMetric(time.Now(), thisSubWindow.SQLUsage)
	if err != nil {
		logutil.BgLogger().Error("Error exists when calling prometheus", zap.Error(err))
	}
	thisSubWindow.SQLUsage.SQLTotal = getSQLSum(&thisSubWindow.SQLUsage.SQLType)
	// logutil.BgLogger().Info(fmt.Sprintf("sqldata", thisSubWindow.SQLUsage.SQLDurationTotal))

	subWindowsLock.Lock()
	rotatedSubWindows = append(rotatedSubWindows, &thisSubWindow)
	if len(rotatedSubWindows) > maxSubWindowLength {
		// Only retain last N sub windows, according to the report interval.
		rotatedSubWindows = rotatedSubWindows[len(rotatedSubWindows)-maxSubWindowLength:]
	}
	subWindowsLock.Unlock()
}

// getWindowData returns data aggregated by window size.
func getWindowData() []*windowData {
	results := make([]*windowData, 0)

	subWindowsLock.RLock()

	i := 0
	for i < len(rotatedSubWindows) {
		thisWindow := *rotatedSubWindows[i]
		var startWindow windowData
		if i == 0 {
			startWindow = thisWindow
		} else {
			startWindow = *rotatedSubWindows[i-1]
		}
		aggregatedSubWindows := 1
		// Aggregate later sub windows
		i++
		for i < len(rotatedSubWindows) && aggregatedSubWindows < maxSubWindowLengthInWindow {
			thisWindow.ExecuteCount += rotatedSubWindows[i].ExecuteCount
			thisWindow.TiFlashUsage.PushDown += rotatedSubWindows[i].TiFlashUsage.PushDown
			thisWindow.TiFlashUsage.ExchangePushDown += rotatedSubWindows[i].TiFlashUsage.ExchangePushDown
			thisWindow.CoprCacheUsage.GTE0 += rotatedSubWindows[i].CoprCacheUsage.GTE0
			thisWindow.CoprCacheUsage.GTE1 += rotatedSubWindows[i].CoprCacheUsage.GTE1
			thisWindow.CoprCacheUsage.GTE10 += rotatedSubWindows[i].CoprCacheUsage.GTE10
			thisWindow.CoprCacheUsage.GTE20 += rotatedSubWindows[i].CoprCacheUsage.GTE20
			thisWindow.CoprCacheUsage.GTE40 += rotatedSubWindows[i].CoprCacheUsage.GTE40
			thisWindow.CoprCacheUsage.GTE80 += rotatedSubWindows[i].CoprCacheUsage.GTE80
			thisWindow.CoprCacheUsage.GTE100 += rotatedSubWindows[i].CoprCacheUsage.GTE100
			thisWindow.SQLUsage.SQLTotal = rotatedSubWindows[i].SQLUsage.SQLTotal - startWindow.SQLUsage.SQLTotal
			thisWindow.SQLUsage.SQLType.Use = rotatedSubWindows[i].SQLUsage.SQLType.Use - startWindow.SQLUsage.SQLType.Use
			thisWindow.SQLUsage.SQLType.Show = rotatedSubWindows[i].SQLUsage.SQLType.Show - startWindow.SQLUsage.SQLType.Show
			thisWindow.SQLUsage.SQLType.Begin = rotatedSubWindows[i].SQLUsage.SQLType.Begin - startWindow.SQLUsage.SQLType.Begin
			thisWindow.SQLUsage.SQLType.Commit = rotatedSubWindows[i].SQLUsage.SQLType.Commit - startWindow.SQLUsage.SQLType.Commit
			thisWindow.SQLUsage.SQLType.Rollback = rotatedSubWindows[i].SQLUsage.SQLType.Rollback - startWindow.SQLUsage.SQLType.Rollback
			thisWindow.SQLUsage.SQLType.Insert = rotatedSubWindows[i].SQLUsage.SQLType.Insert - startWindow.SQLUsage.SQLType.Insert
			thisWindow.SQLUsage.SQLType.Replace = rotatedSubWindows[i].SQLUsage.SQLType.Replace - startWindow.SQLUsage.SQLType.Replace
			thisWindow.SQLUsage.SQLType.Delete = rotatedSubWindows[i].SQLUsage.SQLType.Delete - startWindow.SQLUsage.SQLType.Delete
			thisWindow.SQLUsage.SQLType.Update = rotatedSubWindows[i].SQLUsage.SQLType.Update - startWindow.SQLUsage.SQLType.Update
			thisWindow.SQLUsage.SQLType.Select = rotatedSubWindows[i].SQLUsage.SQLType.Select - startWindow.SQLUsage.SQLType.Select
			thisWindow.SQLUsage.SQLType.Other = rotatedSubWindows[i].SQLUsage.SQLType.Other - startWindow.SQLUsage.SQLType.Other
			thisWindow.SQLUsage.SQLDurationTotal = calculateDeltaB(rotatedSubWindows[i].SQLUsage.SQLDurationTotal, startWindow.SQLUsage.SQLDurationTotal)
			aggregatedSubWindows++
			if aggregatedSubWindows == maxSubWindowLengthInWindow {
				logutil.BgLogger().Info("durstring" + bucketMap2Json(calculateDeltaB(rotatedSubWindows[i].SQLUsage.SQLDurationTotal, startWindow.SQLUsage.SQLDurationTotal)))
			}
			i++
		}
		results = append(results, &thisWindow)
	}

	subWindowsLock.RUnlock()

	return results
}
