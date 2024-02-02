package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	log "github.com/sirupsen/logrus"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/argoproj/argo-rollouts/metricproviders/plugin"
	rolloutsPlugin "github.com/argoproj/argo-rollouts/metricproviders/plugin/rpc"
	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	metricutil "github.com/argoproj/argo-rollouts/utils/metric"
	pluginTypes "github.com/argoproj/argo-rollouts/utils/plugin/types"
	timeutil "github.com/argoproj/argo-rollouts/utils/time"
)

const (
	ResolvedHoneycombQuery = "ResolvedHoneycombQuery"
)

// Implements the Provider Interface
type HoneycombProvider struct {
	api      honeycombAPI
	queryID  string
	rawQuery string
	dataset  string
	LogCtx   log.Entry
}

var _ rolloutsPlugin.MetricProviderPlugin = (*HoneycombProvider)(nil)

type Config struct {
	// Query is a raw honeycomb query to perform
	Query string `json:"query,omitempty" protobuf:"bytes,1,opt,name=query"`
	// Dataset is the name of the honeycomb dataset to query
	Dataset string `json:"dataset,omitempty" protobuf:"bytes,2,opt,name=dataset"`
}

func NewHoneycombProvider(metric v1alpha1.Metric) (*HoneycombProvider, error) {
	config := Config{}
	err := json.Unmarshal(metric.Provider.Plugin["argoproj-labs/honeycomb"], &config)
	if err != nil {
		return nil, err
	}

	return &HoneycombProvider{
		rawQuery: config.Query,
		dataset:  config.Dataset,
	}, nil
}

func (p *HoneycombProvider) InitPlugin() pluginTypes.RpcError {
	config, err := rest.InClusterConfig()
	if err != nil {
		return pluginTypes.RpcError{ErrorString: err.Error()}
	}

	k8sClientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return pluginTypes.RpcError{ErrorString: err.Error()}
	}

	api, err := newHoneycombAPI(p.LogCtx, k8sClientset)
	if err != nil {
		return pluginTypes.RpcError{ErrorString: err.Error()}
	}
	p.api = api

	return pluginTypes.RpcError{}
}

func (p *HoneycombProvider) Type() string {
	return plugin.ProviderType
}

// GetMetadata returns any additional metadata which needs to be stored & displayed as part of the metrics result.
func (p *HoneycombProvider) GetMetadata(metric v1alpha1.Metric) map[string]string {
	metricsMetadata := make(map[string]string)
	if p.rawQuery != "" {
		metricsMetadata[ResolvedHoneycombQuery] = p.rawQuery
	}
	return metricsMetadata
}

// Run queries Honeycomb for the metric data
func (p *HoneycombProvider) Run(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric) v1alpha1.Measurement {
	startTime := timeutil.MetaNow()
	newMeasurement := v1alpha1.Measurement{
		StartedAt: &startTime,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if p.queryID == "" {
		query, err := p.api.CreateQuery(ctx, p.rawQuery, p.dataset)
		if err != nil {
			return metricutil.MarkMeasurementError(newMeasurement, err)
		}

		p.queryID = query.ID
	}

	queryResult, err := p.api.GetQueryResult(ctx, p.queryID, p.dataset)
	if err != nil {
		return metricutil.MarkMeasurementError(newMeasurement, err)
	}

	valueStr, newStatus, err := p.processResponse(metric, queryResult)
	if err != nil {
		return metricutil.MarkMeasurementError(newMeasurement, err)
	}
	newMeasurement.Value = valueStr
	newMeasurement.Phase = newStatus

	finishedTime := timeutil.MetaNow()
	newMeasurement.FinishedAt = &finishedTime
	return newMeasurement
}

type envStruct struct {
	Result int `expr:"result"`
}

func (p *HoneycombProvider) processResponse(metric v1alpha1.Metric, result *QueryResult) (string, v1alpha1.AnalysisPhase, error) {
	if len(result.Data.Results) == 0 {
		return "", v1alpha1.AnalysisPhaseFailed, errors.New("no results returned")
	}

	if len(result.Query.Calculations) == 0 {
		// this shouldn't happen, but just in case
		return "", v1alpha1.AnalysisPhaseFailed, errors.New("no calculations specifed in query")
	}

	var op string
	if result.Query.Calculations[0].Column != nil {
		op = fmt.Sprintf("%s(%s)", result.Query.Calculations[0].Op, *result.Query.Calculations[0].Column)
	} else {
		op = result.Query.Calculations[0].Op
	}

	values := make([]int, len(result.Data.Results))
	valuesStr := make([]string, len(result.Data.Results))

	for i, result := range result.Data.Results {
		resultValue := result.Data[op]
		values[i] = resultValue.(int)
		valuesStr[i] = fmt.Sprintf("%d", resultValue)
	}

	var sb strings.Builder
	sb.WriteString("[")
	sb.WriteString(strings.Join(valuesStr, ", "))
	sb.WriteString("]")
	valueStr := sb.String()

	if metric.SuccessCondition == "" && metric.FailureCondition == "" {
		//Always return success unless there is an error
		return valueStr, v1alpha1.AnalysisPhaseSuccessful, nil
	}

	// evaluate results against success/failure criteria
	var successProgram, failProgram *vm.Program
	var err error
	if metric.SuccessCondition != "" {
		successProgram, err = expr.Compile(metric.SuccessCondition, expr.Env(envStruct{}))
		if err != nil {
			return valueStr, v1alpha1.AnalysisPhaseFailed, err
		}
	}

	if metric.FailureCondition != "" {
		failProgram, err = expr.Compile(metric.FailureCondition, expr.Env(envStruct{}))
		if err != nil {
			return valueStr, v1alpha1.AnalysisPhaseFailed, err
		}
	}

	// apply threshold to the first operator if there are multiple
	successCondition := false
	failCondition := false

	for _, resultValue := range values {
		env := envStruct{
			Result: resultValue,
		}

		if metric.SuccessCondition != "" {
			output, err := expr.Run(successProgram, env)
			if err != nil {
				return valueStr, v1alpha1.AnalysisPhaseError, err
			}

			switch val := output.(type) {
			case bool:
				successCondition = val
			default:
				return valueStr, v1alpha1.AnalysisPhaseError, fmt.Errorf("expected bool, but got %T", val)
			}
		}

		if metric.FailureCondition != "" {
			output, err := expr.Run(failProgram, env)
			if err != nil {
				return valueStr, v1alpha1.AnalysisPhaseError, err
			}

			switch val := output.(type) {
			case bool:
				failCondition = val
			default:
				return valueStr, v1alpha1.AnalysisPhaseError, fmt.Errorf("expected bool, but got %T", val)
			}
		}
	}

	switch {
	case metric.SuccessCondition != "" && metric.FailureCondition == "":
		// Without a failure condition, a measurement is considered a failure if the measurement's success condition is not true
		failCondition = !successCondition
	case metric.SuccessCondition == "" && metric.FailureCondition != "":
		// Without a success condition, a measurement is considered a successful if the measurement's failure condition is not true
		successCondition = !failCondition
	}

	if failCondition {
		return valueStr, v1alpha1.AnalysisPhaseFailed, nil
	}

	if !failCondition && !successCondition {
		return valueStr, v1alpha1.AnalysisPhaseInconclusive, nil
	}

	return valueStr, v1alpha1.AnalysisPhaseSuccessful, nil
}

func (p *HoneycombProvider) Resume(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	return measurement
}

func (p *HoneycombProvider) Terminate(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	return measurement
}

func (p *HoneycombProvider) GarbageCollect(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, i int) pluginTypes.RpcError {
	return pluginTypes.RpcError{}
}
