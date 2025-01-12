// Unless explicitly stated otherwise all files in this repository are licensed
// under the MIT License.
// This product includes software developed at Guance Cloud (https://www.guance.com/).
// Copyright 2021-present Guance, Inc.

// Package jaeger handle Jaeger tracing metrics.
package jaeger

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/goroutine"

	"github.com/GuanceCloud/cliutils"
	"github.com/GuanceCloud/cliutils/logger"
	"github.com/GuanceCloud/cliutils/point"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/datakit"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/httpapi"
	dkio "gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/io"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/plugins/inputs"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/storage"
	itrace "gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/trace"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/workerpool"
	"google.golang.org/protobuf/proto"
)

var (
	_ inputs.InputV2   = &Input{}
	_ inputs.HTTPInput = &Input{}
)

const (
	inputName    = "jaeger"
	sampleConfig = `
[[inputs.jaeger]]
  # Jaeger endpoint for receiving tracing span over HTTP.
  # Default value set as below. DO NOT MODIFY THE ENDPOINT if not necessary.
  endpoint = "/apis/traces"

  # Jaeger agent host:port address for UDP transport.
  # address = "127.0.0.1:6831"
  # binary_address = "127.0.0.1:6832"

  ## ignore_tags will work as a blacklist to prevent tags send to data center.
  ## Every value in this list is a valid string of regular expression.
  # ignore_tags = ["block1", "block2"]

  ## Keep rare tracing resources list switch.
  ## If some resources are rare enough(not presend in 1 hour), those resource will always send
  ## to data center and do not consider samplers and filters.
  # keep_rare_resource = false

  ## delete trace message
  # del_message = true

  ## Ignore tracing resources map like service:[resources...].
  ## The service name is the full service name in current application.
  ## The resource list is regular expressions uses to block resource names.
  ## If you want to block some resources universally under all services, you can set the
  ## service name as "*". Note: double quotes "" cannot be omitted.
  # [inputs.jaeger.close_resource]
    # service1 = ["resource1", "resource2", ...]
    # service2 = ["resource1", "resource2", ...]
    # "*" = ["close_resource_under_all_services"]
    # ...

  ## Sampler config uses to set global sampling strategy.
  ## sampling_rate used to set global sampling rate.
  # [inputs.jaeger.sampler]
    # sampling_rate = 1.0

  # [inputs.jaeger.tags]
    # key1 = "value1"
    # key2 = "value2"
    # ...

  ## Threads config controls how many goroutines an agent cloud start to handle HTTP request.
  ## buffer is the size of jobs' buffering of worker channel.
  ## threads is the total number fo goroutines at running time.
  ## timeout is the duration(ms) before a job can return a result.
  # [inputs.jaeger.threads]
    # buffer = 100
    # threads = 8

  ## Storage config a local storage space in hard dirver to cache trace data.
  ## path is the local file path used to cache data.
  ## capacity is total space size(MB) used to store data.
  # [inputs.jaeger.storage]
    # path = "./jaeger_storage"
    # capacity = 5120
`
)

var (
	log            = logger.DefaultSLogger(inputName)
	afterGatherRun itrace.AfterGatherHandler
	ignoreTags     []*regexp.Regexp
	tags           map[string]string
	wkpool         *workerpool.WorkerPool
	localCache     *storage.Storage
	delMessage     bool
)

type Input struct {
	Path             string                       `toml:"path"`          // deprecated
	UDPAgent         string                       `toml:"udp_agent"`     // deprecated
	Pipelines        map[string]string            `toml:"pipelines"`     // deprecated
	CustomerTags     []string                     `toml:"customer_tags"` // deprecated
	Endpoint         string                       `toml:"endpoint"`
	Address          string                       `toml:"address"`
	BinaryAddress    string                       `toml:"binary_address"`
	IgnoreTags       []string                     `toml:"ignore_tags"`
	DelMessage       bool                         `toml:"del_message"`
	KeepRareResource bool                         `toml:"keep_rare_resource"`
	CloseResource    map[string][]string          `toml:"close_resource"`
	Sampler          *itrace.Sampler              `toml:"sampler"`
	Tags             map[string]string            `toml:"tags"`
	WPConfig         *workerpool.WorkerPoolConfig `toml:"threads"`
	LocalCacheConfig *storage.StorageConfig       `toml:"storage"`

	feeder  dkio.Feeder
	semStop *cliutils.Sem // start stop signal
	Tagger  datakit.GlobalTagger
}

func (*Input) Catalog() string { return inputName }

func (*Input) AvailableArchs() []string { return datakit.AllOS }

func (*Input) SampleConfig() string { return sampleConfig }

func (*Input) SampleMeasurement() []inputs.Measurement {
	return []inputs.Measurement{&itrace.TraceMeasurement{Name: inputName}}
}

func (ipt *Input) RegHTTPHandler() {
	log = logger.SLogger(inputName)

	var err error
	if ipt.WPConfig != nil {
		if wkpool, err = workerpool.NewWorkerPool(ipt.WPConfig, log); err != nil {
			log.Errorf("### new worker-pool failed: %s", err.Error())
		} else if err = wkpool.Start(); err != nil {
			log.Errorf("### start worker-pool failed: %s", err.Error())
		}
	}
	if ipt.LocalCacheConfig != nil {
		if localCache, err = storage.NewStorage(ipt.LocalCacheConfig, log); err != nil {
			log.Errorf("### new local-cache failed: %s", err.Error())
		} else {
			localCache.RegisterConsumer(storage.HTTP_KEY, func(buf []byte) error {
				start := time.Now()
				reqpb := &storage.Request{}
				if err := proto.Unmarshal(buf, reqpb); err != nil {
					return err
				} else {
					req := &http.Request{
						Method:           reqpb.Method,
						Proto:            reqpb.Proto,
						ProtoMajor:       int(reqpb.ProtoMajor),
						ProtoMinor:       int(reqpb.ProtoMinor),
						Header:           storage.ConvertMapEntriesToMap(reqpb.Header),
						Body:             io.NopCloser(bytes.NewBuffer(reqpb.Body)),
						ContentLength:    reqpb.ContentLength,
						TransferEncoding: reqpb.TransferEncoding,
						Close:            reqpb.Close,
						Host:             reqpb.Host,
						Form:             storage.ConvertMapEntriesToMap(reqpb.Form),
						PostForm:         storage.ConvertMapEntriesToMap(reqpb.PostForm),
						RemoteAddr:       reqpb.RemoteAddr,
						RequestURI:       reqpb.RequestUri,
					}
					if req.URL, err = url.Parse(reqpb.Url); err != nil {
						log.Errorf("### parse raw URL: %s failed: %s", reqpb.Url, err.Error())
					}
					handleJaegerTrace(&httpapi.NopResponseWriter{}, req)

					log.Debugf("### process status: buffer-size: %dkb, cost: %dms, err: %v", len(reqpb.Body)>>10, time.Since(start)/time.Millisecond, err)

					return nil
				}
			})
			if err = localCache.RunConsumeWorker(); err != nil {
				log.Errorf("### run local-cache consumer failed: %s", err.Error())
			}
		}
	}

	var afterGather *itrace.AfterGather
	if localCache != nil && localCache.Enabled() {
		afterGather = itrace.NewAfterGather(
			itrace.WithLogger(log),
			itrace.WithRetry(100*time.Millisecond),
			itrace.WithPointOptions(point.WithExtraTags(ipt.Tagger.HostTags())),
			itrace.WithFeeder(ipt.feeder),
		)
	} else {
		afterGather = itrace.NewAfterGather(itrace.WithLogger(log),
			itrace.WithPointOptions(point.WithExtraTags(ipt.Tagger.HostTags())), itrace.WithFeeder(ipt.feeder))
	}
	afterGatherRun = afterGather

	// add filters: the order of appending filters into AfterGather is important!!!
	// the order of appending represents the order of that filter executes.
	// add close resource filter
	if len(ipt.CloseResource) != 0 {
		closeResource := &itrace.CloseResource{}
		closeResource.UpdateIgnResList(ipt.CloseResource)
		afterGather.AppendFilter(closeResource.Close)
	}
	// add error status penetration
	afterGather.AppendFilter(itrace.PenetrateErrorTracing)
	// add rare resource keeper
	if ipt.KeepRareResource {
		keepRareResource := &itrace.KeepRareResource{}
		keepRareResource.UpdateStatus(ipt.KeepRareResource, time.Hour)
		afterGather.AppendFilter(keepRareResource.Keep)
	}
	// add sampler
	if ipt.Sampler != nil && (ipt.Sampler.SamplingRateGlobal >= 0 && ipt.Sampler.SamplingRateGlobal <= 1) {
		sampler := ipt.Sampler.Init()
		afterGather.AppendFilter(sampler.Sample)
	}

	log.Debugf("### register handler for %s of agent %s", ipt.Endpoint, inputName)
	if ipt.Endpoint != "" {
		httpapi.RegHTTPHandler("POST", ipt.Endpoint,
			workerpool.HTTPWrapper(httpStatusRespFunc, wkpool,
				httpapi.HTTPStorageWrapper(storage.HTTP_KEY, httpStatusRespFunc, localCache, handleJaegerTrace)))
	}
}

func (ipt *Input) Run() {
	for _, v := range ipt.IgnoreTags {
		if rexp, err := regexp.Compile(v); err != nil {
			log.Debug(err.Error())
		} else {
			ignoreTags = append(ignoreTags, rexp)
		}
	}
	tags = ipt.Tags
	traceOpts = append(point.CommonLoggingOptions(), point.WithExtraTags(ipt.Tagger.HostTags()))
	delMessage = ipt.DelMessage
	if ipt.Address != "" {
		log.Debugf("### %s UDP agent is starting...", inputName)
		g := goroutine.NewGroup(goroutine.Option{Name: inputName})
		g.Go(func(ctx context.Context) error {
			if err := StartUDPAgent(CompactProtocol, ipt.Address, ipt.semStop); err != nil {
				log.Errorf("### start %s UDP agent failed: %s", inputName, err.Error())
			}
			return nil
		})
	}
	if ipt.BinaryAddress != "" {
		g := goroutine.NewGroup(goroutine.Option{Name: inputName})
		g.Go(func(ctx context.Context) error {
			if err := StartUDPAgent(BinaryProtocol, ipt.BinaryAddress, ipt.semStop); err != nil {
				log.Errorf("### start %s UDP agent failed: %s", inputName, err.Error())
			}
			return nil
		})
	}

	log.Debugf("### %s agent is running...", inputName)

	select {
	case <-datakit.Exit.Wait():
		ipt.exit()
		log.Info("jaeger exit")
		return
	case <-ipt.semStop.Wait():
		ipt.exit()
		log.Info("jaeger return")
		return
	}
}

func (ipt *Input) exit() {
	if wkpool != nil {
		wkpool.Shutdown()
		log.Debug("### workerpool closed")
	}
	if localCache != nil {
		if err := localCache.Close(); err != nil {
			log.Error(err.Error())
		}
		log.Debug("### storage closed")
	}
}

func (ipt *Input) Terminate() {
	if ipt.semStop != nil {
		ipt.semStop.Close()
	}

	if ipt.Endpoint != "" {
		httpapi.RemoveHTTPRoute("POST", ipt.Endpoint)
	}
}

func defaultInput() *Input {
	return &Input{
		feeder:  dkio.DefaultFeeder(),
		semStop: cliutils.NewSem(),
		Tagger:  datakit.DefaultGlobalTagger(),
	}
}

func noinit() { //nolint:gochecknoinits
	inputs.Add(inputName, func() inputs.Input {
		return defaultInput()
	})
}
