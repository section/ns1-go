package ns1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	clientVersion    = "0.9.0"
	defaultEndpoint  = "https://api.nsone.net/v1/"
	defaultUserAgent = "go-ns1/" + clientVersion

	headerAuth          = "X-NSONE-Key"
	headerRateLimit     = "X-Ratelimit-Limit"
	headerRateRemaining = "X-Ratelimit-Remaining"
	headerRatePeriod    = "X-Ratelimit-Period"
)

// Doer is a single method interface that allows a user to extend/augment an http.Client instance.
// Note: http.Client satisfies the Doer interface.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// APIClient stores NS1 client state
type APIClient struct {
	// client handles all http communication.
	// The default value is *http.Client
	client Doer

	// NS1 rest endpoint, overrides default if given.
	Endpoint *url.URL

	// NS1 api key (value for http request header 'X-NSONE-Key').
	ApiKey string

	// NS1 go rest user agent (value for http request header 'User-Agent').
	UserAgent string

	// Func to call after response is returned in Do
	RateLimitFunc func(RateLimit)

	// Enables verbose logs.
	debug bool
}

// New takes an API Key and creates an *APIClient
func New(k string) *APIClient {
	endpoint, _ := url.Parse(defaultEndpoint)
	return &APIClient{
		client:        http.DefaultClient,
		Endpoint:      endpoint,
		ApiKey:        k,
		RateLimitFunc: defaultRateLimitFunc,
		UserAgent:     defaultUserAgent,
	}
}

func NewAPIClient(httpClient Doer, options ...APIClientOption) *APIClient {
	endpoint, _ := url.Parse(defaultEndpoint)

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	c := &APIClient{
		client:        httpClient,
		Endpoint:      endpoint,
		RateLimitFunc: defaultRateLimitFunc,
		UserAgent:     defaultUserAgent,
	}

	for _, option := range options {
		option(c)
	}
	return c
}

// Debug enables debug logging
func (c *APIClient) Debug() {
	c.debug = true
}

type APIClientOption func(*APIClient)

func SetClient(client Doer) APIClientOption {
	return func(c *APIClient) { c.client = client }
}

func SetApiKey(key string) APIClientOption {
	return func(c *APIClient) { c.ApiKey = key }
}

func SetEndpoint(endpoint string) APIClientOption {
	return func(c *APIClient) { c.Endpoint, _ = url.Parse(endpoint) }
}

func SetUserAgent(ua string) APIClientOption {
	return func(c *APIClient) { c.UserAgent = ua }
}

func SetRateLimitFunc(ratefunc func(rl RateLimit)) APIClientOption {
	return func(c *APIClient) { c.RateLimitFunc = ratefunc }
}

func (c APIClient) Do(req *http.Request, v interface{}) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	rl := parseRate(resp)
	c.RateLimitFunc(rl)

	err = CheckResponse(resp)
	if err != nil {
		return resp, err
	}

	if v != nil {
		// Try to decode body into the given type.
		err := json.NewDecoder(resp.Body).Decode(&v)
		if err != nil {
			return nil, err
		}
	}

	return resp, err
}

func (c *APIClient) NewRequest(method, path string, body interface{}) (*http.Request, error) {
	rel, err := url.Parse(path)
	if err != nil {
		return nil, err
	}

	uri := c.Endpoint.ResolveReference(rel)

	// Encode body as json
	buf := new(bytes.Buffer)
	if body != nil {
		err := json.NewEncoder(buf).Encode(body)
		if err != nil {
			return nil, err
		}
	}

	if c.debug {
		log.Printf("[DEBUG] %s: %s (%s)", method, uri.String(), buf)
	}

	req, err := http.NewRequest(method, uri.String(), buf)
	if err != nil {
		return nil, err
	}

	req.Header.Add(headerAuth, c.ApiKey)
	req.Header.Add("User-Agent", c.UserAgent)
	return req, nil
}

// Contains all http responses outside the 2xx range.
type RestError struct {
	Resp    *http.Response
	Message string
}

// Satisfy std lib error interface.
func (re *RestError) Error() string {
	return fmt.Sprintf("%v %v: %d %v", re.Resp.Request.Method, re.Resp.Request.URL, re.Resp.StatusCode, re.Message)
}

// Handles parsing of rest api errors. Returns nil if no error.
func CheckResponse(resp *http.Response) error {
	if c := resp.StatusCode; c >= 200 && c <= 299 {
		return nil
	}

	restError := &RestError{Resp: resp}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return restError
	}

	err = json.Unmarshal(b, restError)
	if err != nil {
		return err
	}

	return restError
}

// Rate limiting strategy for the APIClient instance.
type RateLimitFunc func(RateLimit)

// RateLimit stores X-Ratelimit-* headers
type RateLimit struct {
	Limit     int
	Remaining int
	Period    int
}

var defaultRateLimitFunc = func(rl RateLimit) {}

// PercentageLeft returns the ratio of Remaining to Limit as a percentage
func (rl RateLimit) PercentageLeft() int {
	return rl.Remaining * 100 / rl.Limit
}

// WaitTime returns the time.Duration ratio of Period to Limit
func (rl RateLimit) WaitTime() time.Duration {
	return (time.Second * time.Duration(rl.Period)) / time.Duration(rl.Limit)
}

// WaitTimeRemaining returns the time.Duration ratio of Period to Remaining
func (rl RateLimit) WaitTimeRemaining() time.Duration {
	return (time.Second * time.Duration(rl.Period)) / time.Duration(rl.Remaining)
}

// RateLimitStrategySleep sets RateLimitFunc to sleep by WaitTimeRemaining
func (c *APIClient) RateLimitStrategySleep() {
	c.RateLimitFunc = func(rl RateLimit) {
		remaining := rl.WaitTimeRemaining()
		if c.debug {
			log.Printf("Rate limiting - Limit %d Remaining %d in period %d: Sleeping %dns", rl.Limit, rl.Remaining, rl.Period, remaining)
		}
		time.Sleep(remaining)
	}
}

// parseRate parses rate related headers from http response.
func parseRate(resp *http.Response) RateLimit {
	var rl RateLimit

	if limit := resp.Header.Get(headerRateLimit); limit != "" {
		rl.Limit, _ = strconv.Atoi(limit)
	}
	if remaining := resp.Header.Get(headerRateRemaining); remaining != "" {
		rl.Remaining, _ = strconv.Atoi(remaining)
	}
	if period := resp.Header.Get(headerRatePeriod); period != "" {
		rl.Period, _ = strconv.Atoi(period)
	}

	return rl
}
