// @author Couchbase <info@couchbase.com>
// @copyright 2015 Couchbase, Inc.
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

// Package cbauthimpl contains internal implementation details of
// cbauth. It's APIs are subject to change without notice.
package cbauthimpl

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// TLSRefreshCallback type describes callback for reinitializing TLSConfig when ssl certificate
// or client cert auth setting changes.
type TLSRefreshCallback func() error

// TLSConfig contains tls settings to be used by cbauth clients
// When something in tls config changes user is notified via TLSRefreshCallback
type TLSConfig struct {
	MinVersion               uint16
	CipherSuites             []uint16
	CipherSuiteNames         []string
	CipherSuiteOpenSSLNames  []string
	PreferServerCipherSuites bool
}

type tlsConfigImport struct {
	MinTLSVersion      string
	Ciphers            []uint16
	CipherNames        []string
	CipherOpenSSLNames []string
	CipherOrder        bool
}

// ErrNoAuth is an error that is returned when the user credentials
// are not recognized
var ErrNoAuth = errors.New("Authentication failure")

// ErrCallbackAlreadyRegistered is used to signal that certificate refresh callback is already registered
var ErrCallbackAlreadyRegistered = errors.New("Certificate refresh callback is already registered")

// ErrUserNotFound is used to signal when username can't be extracted from client certificate.
var ErrUserNotFound = errors.New("Username not found")

// Node struct is used as part of Cache messages to describe creds and
// ports of some cluster node.
type Node struct {
	Host     string
	User     string
	Password string
	Ports    []int
	Local    bool
}

func matchHost(n Node, host string) bool {
	NodeHostIP := net.ParseIP(n.Host)
	HostIP := net.ParseIP(host)

	if NodeHostIP.IsLoopback() {
		return true
	}
	if HostIP.IsLoopback() && n.Local {
		return true
	}

	// If both are IP addresses then use the standard API to check if they are equal.
	if NodeHostIP != nil && HostIP != nil {
		return HostIP.Equal(NodeHostIP)
	}
	return host == n.Host
}

func getMemcachedCreds(n Node, host string, port int) (user, password string) {
	if !matchHost(n, host) {
		return "", ""
	}
	for _, p := range n.Ports {
		if p == port {
			return n.User, n.Password
		}
	}
	return "", ""
}

type credsDB struct {
	nodes                  []Node
	authCheckURL           string
	permissionCheckURL     string
	specialUser            string
	specialPassword        string
	permissionsVersion     string
	authVersion            string
	certVersion            int
	extractUserFromCertURL string
	clientCertAuthState    string
	clientCertAuthVersion  string
	tlsConfig              TLSConfig
}

// Cache is a structure into which the revrpc json is unmarshalled
type Cache struct {
	Nodes                  []Node
	AuthCheckURL           string `json:"authCheckUrl"`
	PermissionCheckURL     string `json:"permissionCheckUrl"`
	SpecialUser            string `json:"specialUser"`
	PermissionsVersion     string
	AuthVersion            string
	CertVersion            int
	ExtractUserFromCertURL string          `json:"extractUserFromCertURL"`
	ClientCertAuthState    string          `json:"clientCertAuthState"`
	ClientCertAuthVersion  string          `json:"clientCertAuthVersion"`
	TLSConfig              tlsConfigImport `json:"tlsConfig"`
}

// CredsImpl implements cbauth.Creds interface.
type CredsImpl struct {
	name     string
	domain   string
	password string
	db       *credsDB
	s        *Svc
}

// Name method returns user name (e.g. for auditing)
func (c *CredsImpl) Name() string {
	return c.name
}

// Domain method returns user domain (for auditing)
func (c *CredsImpl) Domain() string {
	switch c.domain {
	case "admin", "ro_admin":
		return "builtin"
	}
	return c.domain
}

// IsAllowed method returns true if the permission is granted
// for these credentials
func (c *CredsImpl) IsAllowed(permission string) (bool, error) {
	return checkPermission(c.s, c.name, c.domain, permission)
}

func verifySpecialCreds(db *credsDB, user, password string) bool {
	return len(user) > 0 && user[0] == '@' && password == db.specialPassword
}

type semaphore chan int

func (s semaphore) signal() {
	<-s
}

func (s semaphore) wait() {
	s <- 1
}

type tlsNotifier struct {
	l        sync.Mutex
	ch       chan struct{}
	callback TLSRefreshCallback
}

func newTLSNotifier() *tlsNotifier {
	return &tlsNotifier{
		ch: make(chan struct{}, 1),
	}
}

func (n *tlsNotifier) notifyTLSChangeLocked() {
	select {
	case n.ch <- struct{}{}:
	default:
	}
}

func (n *tlsNotifier) notifyTLSChange() {
	n.l.Lock()
	defer n.l.Unlock()
	n.notifyTLSChangeLocked()
}

func (n *tlsNotifier) registerCallback(callback TLSRefreshCallback) error {
	n.l.Lock()
	defer n.l.Unlock()

	if n.callback != nil {
		return ErrCallbackAlreadyRegistered
	}

	n.callback = callback
	n.notifyTLSChangeLocked()
	return nil
}

func (n *tlsNotifier) getCallback() TLSRefreshCallback {
	n.l.Lock()
	defer n.l.Unlock()

	return n.callback
}

func (n *tlsNotifier) maybeExecuteCallback() error {
	callback := n.getCallback()

	if callback != nil {
		return callback()
	}
	return nil
}

func (n *tlsNotifier) loop() {
	retry := (<-chan time.Time)(nil)

	for {
		select {
		case <-retry:
			retry = nil
		case <-n.ch:
		}

		err := n.maybeExecuteCallback()

		if err == nil {
			retry = nil
			continue
		}

		if retry == nil {
			retry = time.After(5 * time.Second)
		}
	}
}

// Svc is a struct that holds state of cbauth service.
type Svc struct {
	l                   sync.Mutex
	db                  *credsDB
	staleErr            error
	freshChan           chan struct{}
	upCache             *LRUCache
	upCacheOnce         sync.Once
	authCache           *LRUCache
	authCacheOnce       sync.Once
	clientCertCache     *LRUCache
	clientCertCacheOnce sync.Once
	httpClient          *http.Client
	semaphore           semaphore
	tlsNotifier         *tlsNotifier
}

func cacheToCredsDB(c *Cache) (db *credsDB) {
	db = &credsDB{
		nodes:                  c.Nodes,
		authCheckURL:           c.AuthCheckURL,
		permissionCheckURL:     c.PermissionCheckURL,
		specialUser:            c.SpecialUser,
		permissionsVersion:     c.PermissionsVersion,
		authVersion:            c.AuthVersion,
		certVersion:            c.CertVersion,
		extractUserFromCertURL: c.ExtractUserFromCertURL,
		clientCertAuthState:    c.ClientCertAuthState,
		clientCertAuthVersion:  c.ClientCertAuthVersion,
		tlsConfig:              importTLSConfig(&c.TLSConfig),
	}
	for _, node := range db.nodes {
		if node.Local {
			db.specialPassword = node.Password
			break
		}
	}
	return
}

func updateDBLocked(s *Svc, db *credsDB) {
	s.db = db
	if s.freshChan != nil {
		close(s.freshChan)
		s.freshChan = nil
	}
}

// UpdateDB is a revrpc method that is used by ns_server update cbauth
// state.
func (s *Svc) UpdateDB(c *Cache, outparam *bool) error {
	if outparam != nil {
		*outparam = true
	}
	// BUG(alk): consider some kind of CAS later
	db := cacheToCredsDB(c)
	s.l.Lock()
	tlsUpdated := s.needRefreshTLS(db)
	updateDBLocked(s, db)
	s.l.Unlock()
	if tlsUpdated {
		s.tlsNotifier.notifyTLSChange()
	}
	return nil
}

// ResetSvc marks service's db as stale.
func ResetSvc(s *Svc, staleErr error) {
	if staleErr == nil {
		panic("staleErr must be non-nil")
	}
	s.l.Lock()
	s.staleErr = staleErr
	updateDBLocked(s, nil)
	s.l.Unlock()
}

func staleError(s *Svc) error {
	if s.staleErr == nil {
		panic("impossible Svc state where staleErr is nil!")
	}
	return s.staleErr
}

// NewSVC constructs Svc instance. Period is initial period of time
// where attempts to access stale DB won't cause DBStaleError responses,
// but service will instead wait for UpdateDB call.
func NewSVC(period time.Duration, staleErr error) *Svc {
	return NewSVCForTest(period, staleErr, func(period time.Duration, freshChan chan struct{}, body func()) {
		time.AfterFunc(period, body)
	})
}

// NewSVCForTest constructs Svc isntance.
func NewSVCForTest(period time.Duration, staleErr error, waitfn func(time.Duration, chan struct{}, func())) *Svc {
	if staleErr == nil {
		panic("staleErr must be non-nil")
	}

	s := &Svc{
		staleErr:    staleErr,
		semaphore:   make(semaphore, 10),
		tlsNotifier: newTLSNotifier(),
	}

	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("http.DefaultTransport not an *http.Transport")
	}
	tr := &http.Transport{
		Proxy:                 dt.Proxy,
		DialContext:           dt.DialContext,
		MaxIdleConns:          dt.MaxIdleConns,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       dt.IdleConnTimeout,
		ExpectContinueTimeout: dt.ExpectContinueTimeout,
	}
	SetTransport(s, tr)

	if period != time.Duration(0) {
		s.freshChan = make(chan struct{})
		waitfn(period, s.freshChan, func() {
			s.l.Lock()
			if s.freshChan != nil {
				close(s.freshChan)
				s.freshChan = nil
			}
			s.l.Unlock()
		})
	}

	go s.tlsNotifier.loop()
	return s
}

// SetTransport allows to change RoundTripper for Svc
func SetTransport(s *Svc, rt http.RoundTripper) {
	s.httpClient = &http.Client{Transport: rt}
}

func (s *Svc) needRefreshTLS(db *credsDB) bool {
	return s.db == nil || s.db.certVersion != db.certVersion ||
		s.db.clientCertAuthState != db.clientCertAuthState ||
		!reflect.DeepEqual(s.db.tlsConfig, db.tlsConfig)
}

func fetchDB(s *Svc) *credsDB {
	s.l.Lock()
	db := s.db
	c := s.freshChan
	s.l.Unlock()

	if db != nil || c == nil {
		return db
	}

	// if db is stale try to wait a bit
	<-c
	// double receive doesn't change anything from correctness
	// standpoint (we close channel), but helps a lot for tests
	<-c
	s.l.Lock()
	db = s.db
	s.l.Unlock()

	return db
}

const tokenHeader = "ns-server-ui"

// IsAuthTokenPresent returns true iff ns_server's ui token header
// ("ns-server-ui") is set to "yes". UI is using that header to
// indicate that request is using so called token auth.
func IsAuthTokenPresent(req *http.Request) bool {
	return req.Header.Get(tokenHeader) == "yes"
}

func copyHeader(name string, from, to http.Header) {
	if val := from.Get(name); val != "" {
		to.Set(name, val)
	}
}

func verifyPasswordOnServer(s *Svc, user, password string) (*CredsImpl, error) {
	req, err := http.NewRequest("GET", "http://host/", nil)
	if err != nil {
		panic("Must not happen: " + err.Error())
	}
	req.SetBasicAuth(user, password)
	return VerifyOnServer(s, req.Header)
}

// VerifyOnServer authenticates http request by calling POST /_cbauth REST endpoint
func VerifyOnServer(s *Svc, reqHeaders http.Header) (*CredsImpl, error) {
	db := fetchDB(s)
	if db == nil {
		return nil, staleError(s)
	}

	if s.db.authCheckURL == "" {
		return nil, ErrNoAuth
	}

	s.semaphore.wait()
	defer s.semaphore.signal()

	req, err := http.NewRequest("POST", db.authCheckURL, nil)
	if err != nil {
		panic(err)
	}

	copyHeader(tokenHeader, reqHeaders, req.Header)
	copyHeader("ns-server-auth-token", reqHeaders, req.Header)
	copyHeader("Cookie", reqHeaders, req.Header)
	copyHeader("Authorization", reqHeaders, req.Header)

	rv, err := executeReqAndGetCreds(s, db, req)
	if err != nil {
		return nil, err
	}

	return rv, nil
}

func executeReqAndGetCreds(s *Svc, db *credsDB, req *http.Request) (*CredsImpl, error) {
	hresp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	defer io.Copy(ioutil.Discard, hresp.Body)

	if hresp.StatusCode == 401 {
		return nil, ErrNoAuth
	}

	if hresp.StatusCode != 200 {
		err = fmt.Errorf("Expecting 200 or 401 from ns_server auth endpoint. Got: %s", hresp.Status)
		return nil, err
	}

	body, err := ioutil.ReadAll(hresp.Body)
	if err != nil {
		return nil, err
	}

	resp := struct {
		User, Domain string
	}{}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return nil, err
	}

	rv := CredsImpl{name: resp.User, domain: resp.Domain, db: db, s: s}
	return &rv, nil
}

type userPermission struct {
	version    string
	user       string
	domain     string
	permission string
}

func checkPermission(s *Svc, user, domain, permission string) (bool, error) {
	db := fetchDB(s)
	if db == nil {
		return false, staleError(s)
	}

	s.upCacheOnce.Do(func() { s.upCache = NewLRUCache(1024) })

	key := userPermission{db.permissionsVersion, user, domain, permission}

	allowed, found := s.upCache.Get(key)
	if found {
		return allowed.(bool), nil
	}

	allowedOnServer, err := checkPermissionOnServer(s, db, user, domain, permission)
	if err != nil {
		return false, err
	}
	s.upCache.Set(key, allowedOnServer)
	return allowedOnServer, nil
}

func checkPermissionOnServer(s *Svc, db *credsDB, user, domain, permission string) (bool, error) {
	s.semaphore.wait()
	defer s.semaphore.signal()

	req, err := http.NewRequest("GET", db.permissionCheckURL, nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(db.specialUser, db.specialPassword)

	v := url.Values{}
	v.Set("user", user)
	v.Set("domain", domain)
	v.Set("permission", permission)
	req.URL.RawQuery = v.Encode()

	hresp, err := s.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer hresp.Body.Close()
	defer io.Copy(ioutil.Discard, hresp.Body)

	switch hresp.StatusCode {
	case 200:
		return true, nil
	case 401:
		return false, nil
	}
	return false, fmt.Errorf("Unexpected return code %v", hresp.StatusCode)
}

type userPassword struct {
	version  string
	user     string
	password string
}

type userIdentity struct {
	user   string
	domain string
}

// VerifyPassword verifies given user/password creds against cbauth
// password database. Returns nil, nil if given creds are not
// recognised at all.
func VerifyPassword(s *Svc, user, password string) (*CredsImpl, error) {
	db := fetchDB(s)
	if db == nil {
		return nil, staleError(s)
	}

	if verifySpecialCreds(db, user, password) {
		return &CredsImpl{
			name:     user,
			password: password,
			db:       db,
			s:        s,
			domain:   "admin"}, nil
	}

	s.authCacheOnce.Do(func() { s.authCache = NewLRUCache(256) })

	key := userPassword{db.authVersion, user, password}

	id, found := s.authCache.Get(key)
	if found {
		identity := id.(userIdentity)
		return &CredsImpl{
			name:     identity.user,
			password: password,
			db:       db,
			s:        s,
			domain:   identity.domain}, nil
	}

	rv, err := verifyPasswordOnServer(s, user, password)
	if err != nil {
		return nil, err
	}

	if rv.domain == "admin" || rv.domain == "local" {
		s.authCache.Set(key, userIdentity{rv.name, rv.domain})
	}
	return rv, nil
}

// GetCreds returns service password for given host and port
// together with memcached admin name and http special user.
// Or "", "", "", nil if host/port represents unknown service.
func GetCreds(s *Svc, host string, port int) (memcachedUser, user, pwd string, err error) {
	db := fetchDB(s)
	if db == nil {
		return "", "", "", staleError(s)
	}
	for _, n := range db.nodes {
		memcachedUser, pwd = getMemcachedCreds(n, host, port)
		if memcachedUser != "" {
			user = db.specialUser
			return
		}
	}
	return
}

// RegisterTLSRefreshCallback registers callback for refreshing TLS config
func RegisterTLSRefreshCallback(s *Svc, callback TLSRefreshCallback) error {
	return s.tlsNotifier.registerCallback(callback)
}

// GetClientCertAuthType returns TLS cert type
func GetClientCertAuthType(s *Svc) (tls.ClientAuthType, error) {
	db := fetchDB(s)
	if db == nil {
		return tls.NoClientCert, staleError(s)
	}

	return getAuthType(db.clientCertAuthState), nil
}

func importTLSConfig(cfg *tlsConfigImport) TLSConfig {
	return TLSConfig{
		MinVersion:               minTLSVersion(cfg.MinTLSVersion),
		CipherSuites:             append([]uint16{}, cfg.Ciphers...),
		CipherSuiteNames:         append([]string{}, cfg.CipherNames...),
		CipherSuiteOpenSSLNames:  append([]string{}, cfg.CipherOpenSSLNames...),
		PreferServerCipherSuites: cfg.CipherOrder,
	}
}

// GetTLSConfig returns current tls config that contains cipher suites,
// min TLS version, etc.
func GetTLSConfig(s *Svc) (TLSConfig, error) {
	db := fetchDB(s)
	if db == nil {
		return TLSConfig{}, staleError(s)
	}
	return db.tlsConfig, nil
}

func minTLSVersion(str string) uint16 {
	switch strings.ToLower(str) {
	case "tlsv1":
		return tls.VersionTLS10
	case "tlsv1.1":
		return tls.VersionTLS11
	case "tlsv1.2":
		return tls.VersionTLS12
	default:
		return tls.VersionTLS10
	}
}

func getAuthType(state string) tls.ClientAuthType {
	if state == "enable" {
		return tls.VerifyClientCertIfGiven
	} else if state == "mandatory" {
		return tls.RequireAndVerifyClientCert
	} else {
		return tls.NoClientCert
	}
}

type clienCertHash struct {
	hash    string
	version string
}

// MaybeGetCredsFromCert extracts user's credentials from certificate
// Those returned credentials could be used for calling IsAllowed function
func MaybeGetCredsFromCert(s *Svc, req *http.Request) (*CredsImpl, error) {
	db := fetchDB(s)
	if db == nil {
		return nil, staleError(s)
	}

	// If TLS is nil, then do nothing as it's an http request and not https.
	if req.TLS == nil {
		return nil, nil
	}

	s.clientCertCacheOnce.Do(func() { s.clientCertCache = NewLRUCache(256) })
	state := db.clientCertAuthState

	if state == "disable" || state == "" {
		return nil, nil
	} else if state == "enable" && len(req.TLS.PeerCertificates) == 0 {
		return nil, nil
	} else {
		// The leaf certificate is the one which will have the username
		// encoded into it and it's the first entry in 'PeerCertificates'.
		cert := req.TLS.PeerCertificates[0]

		h := md5.New()
		h.Write(cert.Raw)
		key := clienCertHash{
			hash:    string(h.Sum(nil)),
			version: db.clientCertAuthVersion,
		}

		val, found := s.clientCertCache.Get(key)
		if found {
			ui, _ := val.(*userIdentity)
			creds := &CredsImpl{name: ui.user, domain: ui.domain, db: db, s: s}
			return creds, nil
		}

		creds, _ := getUserIdentityFromCert(cert, db, s)
		if creds != nil {
			ui := &userIdentity{user: creds.name, domain: creds.domain}
			s.clientCertCache.Set(key, interface{}(ui))
			return creds, nil
		}

		return nil, ErrUserNotFound
	}
}

func getUserIdentityFromCert(cert *x509.Certificate, db *credsDB, s *Svc) (*CredsImpl, error) {
	if db.authCheckURL == "" {
		return nil, ErrNoAuth
	}

	s.semaphore.wait()
	defer s.semaphore.signal()

	req, err := http.NewRequest("POST", db.extractUserFromCertURL, bytes.NewReader(cert.Raw))
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/octet-stream")
	req.SetBasicAuth(db.specialUser, db.specialPassword)

	rv, err := executeReqAndGetCreds(s, db, req)
	if err != nil {
		return nil, err
	}

	return rv, nil
}
