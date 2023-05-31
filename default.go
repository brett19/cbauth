// @author Couchbase <info@couchbase.com>
// @copyright 2014 Couchbase, Inc.
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

package cbauth

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/rpc"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/couchbase/cbauth/cbauthimpl"
	"github.com/couchbase/cbauth/revrpc"
)

// Default variable holds default authenticator. Default authenticator
// is constructed automatically from environment variables passed by
// ns_server. It is nil if your process was not (correctly) spawned by
// ns_server.
var Default Authenticator

var errDisconnected = errors.New("revrpc connection to ns_server was closed")

const waitBeforeStale = time.Minute

func runRPCForSvc(rpcsvc *revrpc.Service, svc *cbauthimpl.Svc) error {
	defPolicy := revrpc.DefaultBabysitErrorPolicy.New()
	// error restart policy that we're going to use simply
	// resets service before delegating to default restart
	// policy. That way we always mark service as stale
	// right after some error occurred.
	cbauthPolicy := func(err error) error {
		resetErr := err
		if err == nil {
			resetErr = errDisconnected
		}
		cbauthimpl.ResetSvc(svc, &DBStaleError{resetErr})
		return defPolicy(err)
	}
	return revrpc.BabysitService(func(s *rpc.Server) error {
		return s.RegisterName("AuthCacheSvc", svc)
	}, rpcsvc, revrpc.FnBabysitErrorPolicy(cbauthPolicy))
}

func startDefault(rpcsvc *revrpc.Service, svc *cbauthimpl.Svc) {
	Default = &authImpl{svc}
	go func() {
		panic(runRPCForSvc(rpcsvc, svc))
	}()
}

func init() {
	rpcsvc, err := revrpc.GetDefaultServiceFromEnv("cbauth")
	if err != nil {
		ErrNotInitialized = fmt.Errorf("Unable to initialize cbauth's revrpc: %s", err)
		return
	}
	startDefault(rpcsvc, newSvc())
}

func newSvc() *cbauthimpl.Svc {
	return cbauthimpl.NewSVC(waitBeforeStale, &DBStaleError{})
}

// InitExternal should be used by external cbauth client to enable cbauth
// with limited functionality. Returns false if Default Authenticator was
// already initialized.
func InitExternal(service, mgmtHostPort, user, password string) (bool, error) {
	return doInternalRetryDefaultInitWithService(service,
		mgmtHostPort, user, password, true)
}

// InternalRetryDefaultInit can be used by golang services that are
// willing to perform manual initialization of cbauth (i.e. for easier
// testing). This API is subject to change and should be used only if
// really needed. Returns false if Default Authenticator was already
// initialized.
func InternalRetryDefaultInit(mgmtHostPort, user, password string) (bool, error) {
	service := filepath.Base(os.Args[0])
	return InternalRetryDefaultInitWithService(service, mgmtHostPort, user, password)
}

// InternalRetryDefaultInitWithService can be used by golang services that are
// willing to perform manual initialization of cbauth (i.e. for easier
// testing). This API is subject to change and should be used only if
// really needed. Returns false if Default Authenticator was already
// initialized.
func InternalRetryDefaultInitWithService(service, mgmtHostPort, user, password string) (bool, error) {
	return doInternalRetryDefaultInitWithService(
		service+"-cbauth", mgmtHostPort, user, password, false)
}

func doInternalRetryDefaultInitWithService(service, mgmtHostPort, user,
	password string, external bool) (bool, error) {
	if Default != nil {
		return false, nil
	}
	host, port, err := SplitHostPort(mgmtHostPort)
	if err != nil {
		return false, nil
	}
	var baseurl string
	if external {
		baseurl = fmt.Sprintf("http://%s:%d/auth/v1/%s",
			host, port, service)
	} else {
		baseurl = fmt.Sprintf("http://%s:%d/%s", host, port, service)
	}
	u, err := url.Parse(baseurl)
	if err != nil {
		return false, fmt.Errorf("Failed to parse constructed url `%s': %s", baseurl, err)
	}
	u.User = url.UserPassword(user, password)

	svc := newSvc()
	svc.SetConnectInfo(mgmtHostPort, user, password)

	startDefault(revrpc.MustService(u.String()), svc)

	return true, nil
}

// ErrNotInitialized is used to signal that ns_server environment
// variables are not set, and thus Default authenticator is not
// configured for calls that use default authenticator.
var ErrNotInitialized = errors.New("cbauth was not initialized")

// WithDefault calls given body with default authenticator. If default
// authenticator is not configured, it returns ErrNotInitialized.
func WithDefault(body func(a Authenticator) error) error {
	return WithAuthenticator(nil, body)
}

// WithAuthenticator calls given body with either passed authenticator
// or default authenticator if `a' is nil. ErrNotInitialized is
// returned if a is nil and default authenticator is not configured.
func WithAuthenticator(a Authenticator, body func(a Authenticator) error) error {
	if a == nil {
		a = Default
		if a == nil {
			return ErrNotInitialized
		}
	}
	return body(a)
}

// AuthWebCreds method extracts credentials from given http request
// using default authenticator.
func AuthWebCreds(req *http.Request) (creds Creds, err error) {
	if Default == nil {
		return nil, ErrNotInitialized
	}
	return Default.AuthWebCreds(req)
}

// Auth method constructs credentials from given user and password
// pair. Uses default authenticator.
func Auth(user, pwd string) (creds Creds, err error) {
	if Default == nil {
		return nil, ErrNotInitialized
	}
	return Default.Auth(user, pwd)
}

// GetHTTPServiceAuth returns user/password creds giving "admin"
// access to given http service inside couchbase cluster. Uses default
// authenticator.
func GetHTTPServiceAuth(hostport string) (user, pwd string, err error) {
	if Default == nil {
		return "", "", ErrNotInitialized
	}
	return Default.GetHTTPServiceAuth(hostport)
}

// GetMemcachedServiceAuth returns user/password creds given "admin"
// access to given memcached service. Uses default authenticator.
func GetMemcachedServiceAuth(hostport string) (user, pwd string, err error) {
	if Default == nil {
		return "", "", ErrNotInitialized
	}
	return Default.GetMemcachedServiceAuth(hostport)
}

// RegisterTLSRefreshCallback registers a callback to be called when any field
// of TLS settings change. The callback is called in separate routine
func RegisterTLSRefreshCallback(callback TLSRefreshCallback) error {
	if Default == nil {
		return ErrNotInitialized
	}
	Default.RegisterTLSRefreshCallback(callback)
	return nil
}

func RegisterConfigRefreshCallback(callback ConfigRefreshCallback) error {
	if Default == nil {
		return ErrNotInitialized
	}
	Default.RegisterConfigRefreshCallback(callback)
	return nil
}

// GetClientCertAuthType returns TLS cert type
func GetClientCertAuthType() (tls.ClientAuthType, error) {
	if Default == nil {
		return tls.NoClientCert, ErrNotInitialized
	}
	return Default.GetClientCertAuthType()
}

func GetClusterEncryptionConfig() (ClusterEncryptionConfig, error) {
	if Default == nil {
		return ClusterEncryptionConfig{}, ErrNotInitialized
	}

	return Default.GetClusterEncryptionConfig()
}

func GetLimitsConfig() (LimitsConfig, error) {
	if Default == nil {
		return LimitsConfig{}, ErrNotInitialized
	}

	return Default.GetLimitsConfig()
}

func GetUserLimits(user, domain, service string) (map[string]int, error) {
	if Default == nil {
		return map[string]int{}, ErrNotInitialized
	}

	return Default.GetUserLimits(user, domain, service)
}

func GetUserUuid(user, domain string) (string, error) {
	if Default == nil {
		return "", ErrNotInitialized
	}

	return Default.GetUserUuid(user, domain)
}

// GetTLSConfig returns current tls config that contains cipher suites,
// min TLS version, etc.
func GetTLSConfig() (TLSConfig, error) {
	if Default == nil {
		return TLSConfig{}, ErrNotInitialized
	}
	return Default.GetTLSConfig()
}
