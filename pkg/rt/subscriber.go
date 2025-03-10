package rt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	jsoniter "github.com/json-iterator/go"
	"github.com/peterbourgon/fastly-exporter/pkg/gen"
)

// HTTPClient is a consumer contract for the subscriber.
// It models a concrete http.Client.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// MetadataProvider is a consumer contract for the subscriber.
// It models the service lookup method of an api.Cache.
type MetadataProvider interface {
	Metadata(id string) (name string, version int, found bool)
}

// Subscriber polls rt.fastly.com for a single service ID.
// It emits the received real-time stats data to Prometheus.
type Subscriber struct {
	client      HTTPClient
	token       string
	serviceID   string
	provider    MetadataProvider
	metrics     *gen.Metrics
	postprocess func()
	logger      log.Logger
}

// SubscriberOption provides some additional behavior to a subscriber.
type SubscriberOption func(*Subscriber)

// WithMetadataProvider sets the resolver used to look up service names and
// versions. By default, a no-op metadata resolver is used, which causes each
// service to have its name set to its service ID, and its version set to
// "unknown".
func WithMetadataProvider(p MetadataProvider) SubscriberOption {
	return func(s *Subscriber) { s.provider = p }
}

// WithLogger sets the logger used by the subscriber while running.
// By default, no log events are emitted.
func WithLogger(logger log.Logger) SubscriberOption {
	return func(s *Subscriber) { s.logger = log.With(logger, "service_id", s.serviceID) }
}

// WithPostprocess sets the postprocess function for the subscriber, which is
// invoked after each successful call to the real-time stats API. By default, a
// no-op postprocess function is invoked. This option is only useful for tests.
func WithPostprocess(f func()) SubscriberOption {
	return func(s *Subscriber) { s.postprocess = f }
}

// NewSubscriber returns a ready-to-use subscriber.
// Run must be called to update the metrics.
func NewSubscriber(client HTTPClient, token, serviceID string, metrics *gen.Metrics, options ...SubscriberOption) *Subscriber {
	s := &Subscriber{
		client:      client,
		token:       token,
		serviceID:   serviceID,
		metrics:     metrics,
		provider:    nopMetadataProvider{},
		postprocess: func() {},
		logger:      log.NewNopLogger(),
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// Run polls rt.fastly.com in a hot loop, collecting real-time stats information
// and emitting it to the Prometheus metrics provided to the constructor. The
// method returns when the context is canceled, or a non-recoverable error
// occurs.
func (s *Subscriber) Run(ctx context.Context) error {
	var ts uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		default:
			name, result, delay, newts, fatal := s.query(ctx, ts)
			s.metrics.RealtimeAPIRequestsTotal.WithLabelValues(s.serviceID, name, string(result)).Inc()
			if fatal != nil {
				return fatal
			}
			s.metrics.LastSuccessfulResponse.WithLabelValues(s.serviceID, name).Set(float64(time.Now().Unix()))
			if delay > 0 {
				contextSleep(ctx, delay)
			}
			ts = newts
		}
	}
}

// query rt.fastly.com for the service ID represented by the subscriber, and
// with the provided starting timestamp. The function may block for several
// seconds; cancel the context to provoke early termination. On success, the
// received real-time data is processed, and the Prometheus metrics related to
// the Fastly service are updated.
//
// Returns the current name of the service, the broad class of result of the API
// request, any delay that should pass before query is invoked again, the new
// timestamp that should be provided to the next call to query, and an error.
// Recoverable errors are logged internally and not returned, so any non-nil
// error returned by this method should be considered fatal to the subscriber.
func (s *Subscriber) query(ctx context.Context, ts uint64) (currentName string, result apiResult, delay time.Duration, newts uint64, fatal error) {
	name, ver, found := s.provider.Metadata(s.serviceID)
	version := strconv.Itoa(ver)
	if !found {
		name, version = s.serviceID, "unknown"
	}
	s.metrics.ServiceInfo.WithLabelValues(s.serviceID, name, version).Set(1)

	// rt.fastly.com blocks until it has data to return.
	// It's safe to call in a (single-threaded!) hot loop.
	u := fmt.Sprintf("https://rt.fastly.com/v1/channel/%s/ts/%d", url.QueryEscape(s.serviceID), ts)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return name, apiResultError, 0, ts, fmt.Errorf("error constructing real-time stats API request: %w", err)
	}

	req.Header.Set("Fastly-Key", s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req.WithContext(ctx))
	if err != nil {
		levelForError(s.logger, err).Log("during", "execute request", "err", err)
		return name, apiResultError, time.Second, ts, nil
	}

	var response gen.APIResponse
	if err := jsoniterAPI.NewDecoder(resp.Body).Decode(&response); err != nil {
		resp.Body.Close()
		level.Error(s.logger).Log("during", "decode response", "err", err)
		return name, apiResultError, time.Second, ts, nil
	}
	resp.Body.Close()

	apiErr := response.Error
	if apiErr == "" {
		apiErr = "<none>"
	}

	switch resp.StatusCode {
	case http.StatusOK:
		level.Debug(s.logger).Log("status_code", resp.StatusCode, "response_ts", response.Timestamp, "err", apiErr)
		if strings.Contains(apiErr, "No data available") {
			result = apiResultNoData
		} else {
			result = apiResultSuccess
		}
		gen.Process(&response, s.serviceID, name, version, s.metrics)
		s.postprocess()

	case http.StatusUnauthorized, http.StatusForbidden:
		result = apiResultError
		level.Error(s.logger).Log("status_code", resp.StatusCode, "response_ts", response.Timestamp, "err", apiErr, "msg", "token may be invalid")
		delay = 15 * time.Second

	default:
		result = apiResultUnknown
		level.Error(s.logger).Log("status_code", resp.StatusCode, "response_ts", response.Timestamp, "err", apiErr)
		delay = 5 * time.Second
	}

	return name, result, delay, response.Timestamp, nil
}

//
//
//

var jsoniterAPI = jsoniter.ConfigFastest

type apiResult string

const (
	apiResultUnknown apiResult = "unknown"
	apiResultError   apiResult = "error"
	apiResultNoData  apiResult = "no data"
	apiResultSuccess apiResult = "success"
)

type nopMetadataProvider struct{}

func (nopMetadataProvider) Metadata(string) (string, int, bool) { return "", 0, false }

func contextSleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

//
//
//

var nopLogger = log.NewNopLogger()

func levelForError(base log.Logger, err error) log.Logger {
	switch {
	case errors.Is(err, context.Canceled):
		return level.Debug(base)
	case err != nil:
		return level.Error(base)
	default:
		return nopLogger
	}
}
