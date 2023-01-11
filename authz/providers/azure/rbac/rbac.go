/*
Copyright The Guard Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package rbac

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	auth "go.kubeguard.dev/guard/auth/providers/azure"
	"go.kubeguard.dev/guard/auth/providers/azure/graph"
	"go.kubeguard.dev/guard/authz"
	authzOpts "go.kubeguard.dev/guard/authz/providers/azure/options"
	azureutils "go.kubeguard.dev/guard/util/azure"
	errutils "go.kubeguard.dev/guard/util/error"
	"go.kubeguard.dev/guard/util/httpclient"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	v "gomodules.xyz/x/version"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/klog/v2"
)

const (
	managedClusters           = "Microsoft.ContainerService/managedClusters"
	fleets                    = "Microsoft.ContainerService/fleets"
	connectedClusters         = "Microsoft.Kubernetes/connectedClusters"
	checkAccessPath           = "/providers/Microsoft.Authorization/checkaccess"
	checkAccessAPIVersion     = "2018-09-01-preview"
	remainingSubReadARMHeader = "x-ms-ratelimit-remaining-subscription-reads"
	expiryDelta               = 60 * time.Second
)

type AuthzInfo struct {
	AADEndpoint string
	ARMEndPoint string
}

type reviewResult struct {
	status *authzv1.SubjectAccessReviewStatus
	err    error
}

type void struct{}

// AccessInfo allows you to check user access from MS RBAC
type AccessInfo struct {
	headers   http.Header
	client    *http.Client
	expiresAt time.Time
	// These allow us to mock out the URL for testing
	apiURL *url.URL

	tokenProvider                   graph.TokenProvider
	clusterType                     string
	azureResourceId                 string
	armCallLimit                    int
	skipCheck                       map[string]void
	skipAuthzForNonAADUsers         bool
	allowNonResDiscoveryPathAccess  bool
	useNamespaceResourceScopeFormat bool
	lock                            sync.RWMutex
	operationsMap                   azureutils.OperationsMap
}

var (
	checkAccessThrottled = promauto.NewCounter(prometheus.CounterOpts{
		Name: "guard_azure_checkaccess_throttling_failure_total",
		Help: "No of throttled checkaccess calls.",
	})

	checkAccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "guard_azure_check_access_requests_total",
			Help: "Number of checkaccess request calls.",
		},
		[]string{"code"},
	)

	checkAccessFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "guard_azure_checkaccess_failure_total",
			Help: "No of checkaccess failures",
		},
		[]string{"code"},
	)

	checkAccessSucceeded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "guard_azure_checkaccess_success_total",
		Help: "Number of successful checkaccess calls.",
	})

	// checkAccessDuration is partitioned by the HTTP status code It uses custom
	// buckets based on the expected request duration.
	checkAccessDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "request_duration_seconds",
			Help:    "A histogram of latencies for requests.",
			Buckets: []float64{.25, .5, 1, 2.5, 5, 10, 15, 20},
		},
		[]string{"code"},
	)

	CheckAccessErrorFormat = "Error occured during authorization check. Please retry again. Error: %s"
)

func getClusterType(clsType string) string {
	switch clsType {
	case authzOpts.ARCAuthzMode:
		return connectedClusters
	case authzOpts.AKSAuthzMode:
		return managedClusters
	case authzOpts.FleetAuthzMode:
		return fleets
	default:
		return ""
	}
}

func newAccessInfo(tokenProvider graph.TokenProvider, rbacURL *url.URL, opts authzOpts.Options, operationsMap azureutils.OperationsMap) (*AccessInfo, error) {
	u := &AccessInfo{
		client: httpclient.DefaultHTTPClient,
		headers: http.Header{
			"Content-Type": []string{"application/json"},
			"User-Agent":   []string{fmt.Sprintf("guard-%s-%s-%s-%s", v.Version.Platform, v.Version.GoVersion, v.Version.Version, opts.AuthzMode)},
		},
		apiURL:                          rbacURL,
		tokenProvider:                   tokenProvider,
		azureResourceId:                 opts.ResourceId,
		armCallLimit:                    opts.ARMCallLimit,
		skipAuthzForNonAADUsers:         opts.SkipAuthzForNonAADUsers,
		allowNonResDiscoveryPathAccess:  opts.AllowNonResDiscoveryPathAccess,
		useNamespaceResourceScopeFormat: opts.UseNamespaceResourceScopeFormat,
	}

	u.skipCheck = make(map[string]void, len(opts.SkipAuthzCheck))
	var member void
	for _, s := range opts.SkipAuthzCheck {
		u.skipCheck[strings.ToLower(s)] = member
	}

	u.clusterType = getClusterType(opts.AuthzMode)

	u.operationsMap = operationsMap

	u.lock = sync.RWMutex{}

	return u, nil
}

func New(opts authzOpts.Options, authopts auth.Options, authzInfo *AuthzInfo, operationsMap azureutils.OperationsMap) (*AccessInfo, error) {
	rbacURL, err := url.Parse(authzInfo.ARMEndPoint)
	if err != nil {
		return nil, err
	}

	var tokenProvider graph.TokenProvider
	switch opts.AuthzMode {
	case authzOpts.ARCAuthzMode:
		tokenProvider = graph.NewClientCredentialTokenProvider(authopts.ClientID, authopts.ClientSecret,
			fmt.Sprintf("%s%s/oauth2/v2.0/token", authzInfo.AADEndpoint, authopts.TenantID),
			fmt.Sprintf("%s.default", authzInfo.ARMEndPoint))
	case authzOpts.FleetAuthzMode:
		tokenProvider = graph.NewAKSTokenProvider(opts.AKSAuthzTokenURL, authopts.TenantID)
	case authzOpts.AKSAuthzMode:
		tokenProvider = graph.NewAKSTokenProvider(opts.AKSAuthzTokenURL, authopts.TenantID)
	}

	return newAccessInfo(tokenProvider, rbacURL, opts, operationsMap)
}

func (a *AccessInfo) RefreshToken() error {
	a.lock.Lock()
	defer a.lock.Unlock()
	if a.IsTokenExpired() {
		resp, err := a.tokenProvider.Acquire("")
		if err != nil {
			klog.Errorf("%s failed to refresh token : %s", a.tokenProvider.Name(), err.Error())
			return errors.Wrap(err, "failed to refresh rbac token")
		}

		// Set the authorization headers for future requests
		a.headers.Set("Authorization", fmt.Sprintf("Bearer %s", resp.Token))
		expIn := time.Duration(resp.Expires) * time.Second
		a.expiresAt = time.Now().Add(expIn - expiryDelta)
		klog.Infof("Token refreshed successfully on %s. Expire at:%s", time.Now(), a.expiresAt)
	}

	return nil
}

func (a *AccessInfo) IsTokenExpired() bool {
	return a.expiresAt.Before(time.Now())
}

func (a *AccessInfo) ShouldSkipAuthzCheckForNonAADUsers() bool {
	return a.skipAuthzForNonAADUsers
}

func (a *AccessInfo) GetResultFromCache(request *authzv1.SubjectAccessReviewSpec, store authz.Store) (bool, bool) {
	var result bool
	key := getResultCacheKey(request)
	klog.V(10).Infof("Cache search for key: %s", key)
	found, _ := store.Get(key, &result)

	if found {
		if result {
			klog.V(5).Infof("cache hit: returning allowed for key %s", key)
		} else {
			klog.V(5).Infof("cache hit: returning denied for key %s", key)
		}
	}

	return found, result
}

func (a *AccessInfo) SkipAuthzCheck(request *authzv1.SubjectAccessReviewSpec) bool {
	if a.clusterType == connectedClusters {
		_, ok := a.skipCheck[strings.ToLower(request.User)]
		return ok
	}
	return false
}

func (a *AccessInfo) SetResultInCache(request *authzv1.SubjectAccessReviewSpec, result bool, store authz.Store) error {
	key := getResultCacheKey(request)
	klog.V(5).Infof("Cache set for key: %s, value: %t", key, result)
	return store.Set(key, result)
}

func (a *AccessInfo) AllowNonResPathDiscoveryAccess(request *authzv1.SubjectAccessReviewSpec) bool {
	if request.NonResourceAttributes != nil && a.allowNonResDiscoveryPathAccess && strings.EqualFold(request.NonResourceAttributes.Verb, "get") {
		path := strings.ToLower(request.NonResourceAttributes.Path)
		if strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/openapi") || strings.HasPrefix(path, "/version") || strings.HasPrefix(path, "/healthz") {
			return true
		}
	}
	return false
}

func (a *AccessInfo) setReqHeaders(req *http.Request) {
	a.lock.RLock()
	defer a.lock.RUnlock()
	// Set the auth headers for the request
	if req.Header == nil {
		req.Header = make(http.Header)
	}

	for k, value := range a.headers {
		req.Header[k] = value
	}
}

func (a *AccessInfo) CheckAccess(request *authzv1.SubjectAccessReviewSpec) (*authzv1.SubjectAccessReviewStatus, error) {
	checkAccessBodies, err := prepareCheckAccessRequestBody(request, a.clusterType, a.operationsMap, a.azureResourceId, a.useNamespaceResourceScopeFormat)
	if err != nil {
		return nil, errors.Wrap(err, "error in preparing check access request")
	}

	checkAccessURL := *a.apiURL
	// Append the path for azure cluster resource id
	checkAccessURL.Path = path.Join(checkAccessURL.Path, a.azureResourceId)
	exist, nameSpaceString := getNameSpaceScope(request, a.useNamespaceResourceScopeFormat)
	if exist {
		checkAccessURL.Path = path.Join(checkAccessURL.Path, nameSpaceString)
	}

	checkAccessURL.Path = path.Join(checkAccessURL.Path, checkAccessPath)
	params := url.Values{}
	params.Add("api-version", checkAccessAPIVersion)
	checkAccessURL.RawQuery = params.Encode()

	var wg sync.WaitGroup // New wait group

	ch := make(chan reviewResult, len(checkAccessBodies))
	if len(checkAccessBodies) > 1 {
		klog.V(5).Infof("Number of checkaccess requests to make: %d", len(checkAccessBodies))
	}
	for _, checkAccessBody := range checkAccessBodies {
		wg.Add(1)
		go a.sendCheckAccessRequest(checkAccessURL, checkAccessBody, &wg, ch)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var finalResult *authzv1.SubjectAccessReviewStatus
	for result := range ch {
		if result.err != nil {
			return nil, result.err
		}

		if result.status.Denied {
			finalResult = result.status
			break
		}

		finalResult = result.status
	}

	return finalResult, nil
}

func (a *AccessInfo) sendCheckAccessRequest(ctx context.Context, checkAccessURL url.URL, checkAccessBody *CheckAccessRequest, ch chan reviewResult) error {
	//defer wg.Done()
	reviewResult := reviewResult{}
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(checkAccessBody); err != nil {
		reviewResult.err = errutils.WithCode(errors.Wrap(err, "error encoding check access request"), http.StatusInternalServerError)
		ch <- reviewResult
		return
	}

	if klog.V(10).Enabled() {
		binaryData, _ := json.MarshalIndent(checkAccessBody, "", "    ")
		klog.V(10).Infof("checkAccessURI:%s", checkAccessURL.String())
		klog.V(10).Infof("binary data:%s", binaryData)
	}

	req, err := http.NewRequest(http.MethodPost, checkAccessURL.String(), buf)
	if err != nil {
		reviewResult.err = errutils.WithCode(errors.Wrap(err, "error creating check access request"), http.StatusInternalServerError)
		ch <- reviewResult
		return
	}

	a.setReqHeaders(req)
	// start time to calculate checkaccess duration
	start := time.Now()
	resp, err := a.client.Do(req)
	duration := time.Since(begin).Seconds()
	if err != nil {
		reviewResult.err = errutils.WithCode(errors.Wrap(err, "error in check access request execution"), http.StatusInternalServerError)
		checkAccessTotal.WithLabelValues(http.StatusInternalServerError).Inc()
		checkAccessDuration.WithLabelValues(http.StatusInternalServerError).Observe(duration)
		ch <- reviewResult
		return
	}

	defer resp.Body.Close()

	checkAccessTotal.WithLabelValues(resp.StatusCode).Inc()
	checkAccessDuration.WithLabelValues(resp.StatusCode).Observe(duration)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		reviewResult.err = errutils.WithCode(errors.Wrap(err, "error in reading response body"), http.StatusInternalServerError)
		ch <- reviewResult
		return
	}

	klog.V(7).Infof("checkaccess response: %s, Configured ARM call limit: %d", string(data), a.armCallLimit)
	if resp.StatusCode != http.StatusOK {
		klog.Errorf("error in check access response. error code: %d, response: %s", resp.StatusCode, string(data))
		// metrics for calls with StatusCode >= 300
		if resp.StatusCode >= http.StatusMultipleChoices {
			if resp.StatusCode == http.StatusTooManyRequests {
				klog.V(10).Infoln("Closing idle TCP connections.")
				a.client.CloseIdleConnections()
				checkAccessThrottled.Inc()
			}

			checkAccessFailed.WithLabelValues(resp.StatusCode).Inc()
		}

		reviewResult.err = errutils.WithCode(errors.Errorf("request %s failed with status code: %d and response: %s", req.URL.Path, resp.StatusCode, string(data)), resp.StatusCode)
		ch <- reviewResult
		return
	} else {
		remaining := resp.Header.Get(remainingSubReadARMHeader)
		klog.Infof("Remaining request count in ARM instance:%s", remaining)
		count, _ := strconv.Atoi(remaining)
		if count < a.armCallLimit {
			if klog.V(10).Enabled() {
				klog.V(10).Infoln("Closing idle TCP connections.")
			}
			// Usually ARM connections are cached by destination ip and port
			// By closing the idle connection, a new request will use different port which
			// will connect to different ARM instance of the region to ensure there is no ARM throttling
			a.client.CloseIdleConnections()
		}
		checkAccessSucceeded.Inc()
	}

	// Decode response and prepare k8s response
	reviewResult.status, reviewResult.err = ConvertCheckAccessResponse(data)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- reviewResult:
		return nil
		// do nothing
	}
}
