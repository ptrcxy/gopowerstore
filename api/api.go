/*
 *
 * Copyright © 2020 Dell Inc. or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	debug = false
)

const paginationHeader = "content-range"

// RequestConfig provide options for the request
type RequestConfig struct {
	// http method Name
	Method string
	// target endpoint
	Endpoint string
	// id of the entity
	ID string
	// action which perform on entity
	Action string
	// addition query params
	QueryParams QueryParamsEncoder
	// request body
	Body interface{}
}

// RenderRequestConfig is RequestConfigRenderer implementation
func (rc RequestConfig) RenderRequestConfig() RequestConfig {
	return rc
}

// RequestConfigRenderer provides methods for rendering request config
type RequestConfigRenderer interface {
	RenderRequestConfig() RequestConfig
}

// PaginationInfo stores information about pagination
type PaginationInfo struct {
	// first element index in response
	First int
	// last element index in response
	Last int
	// total elements count
	Total int
	// indicate that response is paginated
	IsPaginate bool
}

// RespMeta struct represents additional information about response
type RespMeta struct {
	// http status
	Status int
	// pagination data
	Pagination PaginationInfo
}

// ApiClient is PowerStore API client interface
type ApiClient interface {
	Traceable
	QueryBulk(ctx context.Context) ([]byte, error)
	Query(
		ctx context.Context,
		cfg RequestConfigRenderer,
		resp interface{}) (RespMeta, error)
	QueryParams() QueryParamsEncoder
	QueryParamsWithFields(provider FieldProvider) QueryParamsEncoder
	SetCustomHTTPHeaders(headers http.Header)
	SetLogger(logger Logger)
}

// FieldProvider provide method which return required fields list
type FieldProvider interface {
	Fields() []string
}

// ClientIMPL struct holds API client settings
type ClientIMPL struct {
	apiURL            string
	insecure          bool
	username          string
	password          string
	httpClient        *http.Client
	defaultTimeout    uint64
	rateLimit         uint64
	requestIDKey      string
	customHTTPHeaders http.Header
	logger            Logger
	apiThrottle       TimeoutSemaphoreInterface
}

// New creates and initialize API client
func New(apiURL string, username string,
	password string, insecure bool, defaultTimeout, rateLimit uint64, requestIDKey string) (*ClientIMPL, error) {
	debug, _ = strconv.ParseBool(os.Getenv("GOPOWERSTORE_DEBUG"))
	if apiURL == "" || username == "" || password == "" {
		return nil, errors.New("API ApiClient can't be initialized: " +
			"Missing endpoint, username, or password param")
	}

	var client *http.Client
	// #nosec G402
	if insecure {
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: insecure,
				},
			},
		}
	} else {
		client = &http.Client{}
	}

	throttle := NewTimeoutSemaphore(int(defaultTimeout), int(rateLimit), &defaultLogger{})

	return &ClientIMPL{
		apiURL:         apiURL,
		insecure:       insecure,
		username:       username,
		password:       password,
		httpClient:     client,
		defaultTimeout: defaultTimeout,
		requestIDKey:   requestIDKey,
		logger:         &defaultLogger{},
		apiThrottle:    throttle,
	}, nil
}

const errorSeverity = "Error"

type apiErrorMsg struct {
	Messages *[]ErrorMsg `json:"messages"`
}

// ErrorMsg is internal error representation
type ErrorMsg struct {
	StatusCode int    `json:"-"`
	ErrorCode  string `json:"code"`
	Severity   string
	Message    string `json:"message_l10n"`
	Arguments  []string
}

func (err *ErrorMsg) Error() string {
	return fmt.Sprintf("%s: %s", err.ErrorCode, err.Message)
}

func buildError(r *http.Response) *ErrorMsg {
	apiErrorMsg := apiErrorMsg{}

	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&apiErrorMsg)
	if err != nil || apiErrorMsg.Messages == nil {
		errMsg := "Unknown error"
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(dec.Buffered()); err == nil {
			s := buf.String()
			errMsg = fmt.Sprintf("%s: %s", errMsg, s)
		}
		return &ErrorMsg{StatusCode: r.StatusCode, Severity: errorSeverity,
			Message: errMsg}
	}
	firstErrMsg := (*apiErrorMsg.Messages)[0]
	firstErrMsg.StatusCode = r.StatusCode
	return &firstErrMsg
}

// SetCustomHTTPHeaders method register headers which will be sent with every request
func (c *ClientIMPL) SetCustomHTTPHeaders(headers http.Header) {
	c.customHTTPHeaders = headers
}

// SetLogger set logger for use by gopowerstore
func (c *ClientIMPL) SetLogger(logger Logger) {
	c.logger = logger
	c.apiThrottle.SetLogger(logger)
}

const (
	bulkApiEnable = "latest_five_min_metrics/enable"
	bulkApiDownload = "latest_five_min_metrics/download"
)

//QueryBulk method call bulk api
func (c *ClientIMPL) QueryBulk(
	ctx context.Context) ([]byte, error) {

	var cancelFuncPtr *func()
	ctx, cancelFuncPtr = c.setupContext(ctx)
	if cancelFuncPtr != nil {
		defer (*cancelFuncPtr)()
	}

	traceMsg := c.prepareTraceMsg(ctx)

	requestURL, err := c.prepareRequestURL(bulkApiEnable, "", "", nil)
	if err != nil {
		return nil, err
	}

	req, err := c.prepareRequest(ctx, "POST", requestURL, traceMsg, nil)
	if err != nil {
		return nil, err
	}

	if err := c.apiThrottle.Acquire(ctx); err != nil {
		return nil, err
	}
	defer c.apiThrottle.Release(ctx)

	r, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	//Then download
	header := make(http.Header)
	header.Add("Accept", "*/*")
	header.Add("Accept-Encoding", "gzip, deflate, br")
	header.Add("If-None-Match", "esa.tar.gz")
	header.Add("Content-Length", "0")
	header.Add("Connection", "keep-alive")

	c.customHTTPHeaders = header

	requestURL, err = c.prepareRequestURL(bulkApiDownload, "", "", nil)
	if err != nil {
		return nil, err
	}

	req, err = c.prepareRequest(ctx, "POST", requestURL, traceMsg, nil)
	if err != nil {
		return nil, err
	}

	if err := c.apiThrottle.Acquire(ctx); err != nil {
		return nil, err
	}
	defer c.apiThrottle.Release(ctx)

	c.customHTTPHeaders = nil

	r, err = c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()

	switch {
	case r.StatusCode >= 200 && r.StatusCode < 300:
		resp, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		return resp, nil
	default:
		return nil, buildError(r)
	}
}

// Query method do http request and reads response to provided struct
func (c *ClientIMPL) Query(
	ctx context.Context,
	cfg RequestConfigRenderer,
	resp interface{}) (RespMeta, error) {

	config := cfg.RenderRequestConfig()
	meta := RespMeta{}
	var cancelFuncPtr *func()
	ctx, cancelFuncPtr = c.setupContext(ctx)
	if cancelFuncPtr != nil {
		defer (*cancelFuncPtr)()
	}

	traceMsg := c.prepareTraceMsg(ctx)

	requestURL, err := c.prepareRequestURL(config.Endpoint, config.ID, config.Action, config.QueryParams)
	if err != nil {
		return meta, err
	}

	req, err := c.prepareRequest(ctx, config.Method, requestURL, traceMsg, config.Body)
	if err != nil {
		return meta, err
	}

	if err := c.apiThrottle.Acquire(ctx); err != nil {
		return meta, err
	}
	defer c.apiThrottle.Release(ctx)

	r, err := c.httpClient.Do(req)
	if err != nil {
		return meta, err
	}
	defer r.Body.Close()

	if debug {
		dump, _ := httputil.DumpResponse(r, true)
		replacedHeader := prepareHTTPDump(dump) // Replace sensitive parts of response headers
		c.logger.Debug(ctx, "%sRESPONSE: %v\n", traceMsg, replacedHeader)
	}
	meta.Status = r.StatusCode
	switch {
	case resp == nil:
		return meta, nil
	case r.StatusCode >= 200 && r.StatusCode < 300:
		c.updatePaginationInfoInMeta(&meta, r)
		err = json.NewDecoder(r.Body).Decode(resp)
		if err == io.EOF {
			return meta, nil
		}
		return meta, err
	default:
		return meta, buildError(r)
	}

}

func addMetaData(req *http.Request, body interface{}) {
	if req == nil || body == nil {
		return
	}
	// If the body contains a MetaData method, extract the data
	// and add as HTTP headers.
	if vp, ok := body.(interface {
		MetaData() http.Header
	}); ok {
		if req.Header == nil {
			req.Header = http.Header{}
		}
		for k := range vp.MetaData() {
			req.Header.Add(k, vp.MetaData().Get(k))
		}
	}
}

// QueryParams method returns QueryParamsEncoder
func (c *ClientIMPL) QueryParams() QueryParamsEncoder {
	return &QueryParams{}
}

// QueryParamsWithFields method returns QueryParamsEncoder with configured select values
func (c *ClientIMPL) QueryParamsWithFields(fp FieldProvider) QueryParamsEncoder {
	return c.QueryParams().Select(fp.Fields()...)
}

func (c *ClientIMPL) prepareRequestURL(endpoint, id string, action string,
	queryParams QueryParamsEncoder) (string, error) {
	requestURL, err := url.Parse(c.apiURL)
	if err != nil {
		return "", err
	}
	endpointFullPath := path.Join(requestURL.Path, endpoint)
	if id != "" {
		endpointFullPath = path.Join(endpointFullPath, id)
	}
	if action != "" {
		endpointFullPath = path.Join(endpointFullPath, action)
	}
	requestURL.Path = endpointFullPath

	if queryParams != nil {
		requestURL.RawQuery = queryParams.Encode()
	}

	return requestURL.String(), nil
}

func (c *ClientIMPL) prepareRequest(ctx context.Context, method, requestURL, traceMsg string,
	body interface{}) (*http.Request, error) {
	var req *http.Request
	var err error
	if body != nil && !(reflect.ValueOf(body).Kind() == reflect.Ptr && reflect.ValueOf(body).IsNil()) {
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err = http.NewRequest(method, requestURL, bytes.NewBuffer(bodyJSON))
		if err != nil {
			return nil, err
		}
	} else {
		req, err = http.NewRequest(method, requestURL, nil)
		if err != nil {
			return nil, err
		}
	}
	req = req.WithContext(ctx)
	req.SetBasicAuth(c.username, c.password)
	for key, values := range c.customHTTPHeaders {
		for _, elem := range values {
			req.Header.Add(key, elem)
		}
	}
	addMetaData(req, body)
	if debug {
		if requestData, err := httputil.DumpRequest(req, true); err == nil {
			c.logger.Debug(ctx, "%sREQUEST: %s", traceMsg, prepareHTTPDump(requestData))
		}
	}
	return req, nil
}

func (c *ClientIMPL) prepareTraceMsg(ctx context.Context) string {
	traceID := c.TraceID(ctx)
	if len(traceID) > 0 {
		return fmt.Sprintf("[%s] ", traceID)
	}
	return ""
}

func (c *ClientIMPL) setupContext(ctx context.Context) (context.Context, *func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, timeoutIsSet := ctx.Deadline()
	if !timeoutIsSet {
		var f func()
		ctx, f = context.WithTimeout(ctx, time.Duration(c.defaultTimeout)*time.Second)
		return ctx, &f
	}
	return ctx, nil
}

func (c *ClientIMPL) updatePaginationInfoInMeta(meta *RespMeta, r *http.Response) {
	if r.StatusCode == 206 {
		paginationStr := r.Header.Get(paginationHeader)
		if paginationStr == "" {
			return
		}
		splittedPaginationStr := strings.Split(paginationStr, "/")
		if len(splittedPaginationStr) != 2 {
			return
		}
		paginationRangeStr, paginationTotalStr := splittedPaginationStr[0], splittedPaginationStr[1]
		splittedRange := strings.Split(paginationRangeStr, "-")
		if len(splittedRange) != 2 {
			return
		}
		firstStr, lastStr := splittedRange[0], splittedRange[1]
		var err error
		var first, last, total int
		first, err = strconv.Atoi(firstStr)
		if err != nil {
			return
		}
		last, err = strconv.Atoi(lastStr)
		if err != nil {
			return
		}
		total, err = strconv.Atoi(paginationTotalStr)
		if err != nil {
			return
		}
		meta.Pagination = PaginationInfo{First: first, Last: last, Total: total, IsPaginate: true}
	}
}

func prepareHTTPDump(dump []byte) string {
	content := replaceSensitiveHeaderInfo(dump)
	return newlineRegexp.ReplaceAllString(content, " ")
}

var newlineRegexp = regexp.MustCompile(`\r?\n`)

var sensitiveDataRegexp = regexp.MustCompile(
	`(?m)(Dell-Emc-Token: |Authorization: )([^\n]+)|(auth_cookie=)([^;]+)`)

func replaceSensitiveHeaderInfo(dump []byte) string {
	return sensitiveDataRegexp.ReplaceAllString(string(dump), "$1$3******")
}
