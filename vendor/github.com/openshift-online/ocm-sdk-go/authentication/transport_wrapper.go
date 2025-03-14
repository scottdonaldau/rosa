/*
Copyright (c) 2021 Red Hat, Inc.

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

// This file contains the implementations of a transport wrapper that implements token
// authentication.

package authentication

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/openshift-online/ocm-sdk-go/internal"
	"github.com/openshift-online/ocm-sdk-go/logging"
	"github.com/prometheus/client_golang/prometheus"
)

// Default values:
const (
	// #nosec G101
	DefaultTokenURL     = "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/token"
	DefaultClientID     = "cloud-services"
	DefaultClientSecret = ""
)

// DefaultScopes is the ser of scopes used by default:
var DefaultScopes = []string{
	"openid",
}

// TransportWrapperBuilder contains the data and logic needed to add to requests the authorization
// token. Don't create objects of this type directly; use the NewTransportWrapper function instead.
type TransportWrapperBuilder struct {
	// Fields used for basic functionality:
	logger            logging.Logger
	tokenURL          string
	clientID          string
	clientSecret      string
	user              string
	password          string
	tokens            []string
	scopes            []string
	agent             string
	trustedCAs        []interface{}
	insecure          bool
	transportWrappers []func(http.RoundTripper) http.RoundTripper

	// Fields used for metrics:
	metricsSubsystem  string
	metricsRegisterer prometheus.Registerer
}

// TransportWrapper contains the data and logic needed to wrap an HTTP round tripper with another
// one that adds authorization tokens to requests.
type TransportWrapper struct {
	// Fields used for basic functionality:
	logger         logging.Logger
	clientID       string
	clientSecret   string
	user           string
	password       string
	scopes         []string
	agent          string
	clientSelector *internal.ClientSelector
	tokenURL       string
	tokenServer    *internal.ServerAddress
	tokenMutex     *sync.Mutex
	tokenParser    *jwt.Parser
	accessToken    *jwt.Token
	refreshToken   *jwt.Token

	// Fields used for metrics:
	metricsSubsystem    string
	metricsRegisterer   prometheus.Registerer
	tokenCountMetric    *prometheus.CounterVec
	tokenDurationMetric *prometheus.HistogramVec
}

// roundTripper is a round tripper that adds authorization tokens to requests.
type roundTripper struct {
	owner     *TransportWrapper
	logger    logging.Logger
	transport http.RoundTripper
}

// Make sure that we implement the interface:
var _ http.RoundTripper = (*roundTripper)(nil)

// NewTransportWrapper creates a new builder that can then be used to configure and create a new
// authentication round tripper.
func NewTransportWrapper() *TransportWrapperBuilder {
	return &TransportWrapperBuilder{}
}

// Logger sets the logger that will be used by the wrapper and by the transports that it creates.
func (b *TransportWrapperBuilder) Logger(value logging.Logger) *TransportWrapperBuilder {
	b.logger = value
	return b
}

// TokenURL sets the URL that will be used to request OpenID access tokens. The default is
// `https://sso.redhat.com/auth/realms/cloud-services/protocol/openid-connect/token`.
func (b *TransportWrapperBuilder) TokenURL(url string) *TransportWrapperBuilder {
	b.tokenURL = url
	return b
}

// Client sets OpenID client identifier and secret that will be used to request OpenID tokens. The
// default identifier is `cloud-services`. The default secret is the empty string. When these two
// values are provided and no user name and password is provided, the round trippers will use the
// client credentials grant to obtain the token. For example, to create a connection using the
// client credentials grant do the following:
//
//	// Use the client credentials grant:
//	wrapper, err := authentication.NewTransportWrapper().
//		Client("myclientid", "myclientsecret").
//		Build()
//
// Note that some OpenID providers (Keycloak, for example) require the client identifier also for
// the resource owner password grant. In that case use the set only the identifier, and let the
// secret blank. For example:
//
//	// Use the resource owner password grant:
//	wrapper, err := authentication.NewTransportWrapper().
//		User("myuser", "mypassword").
//		Client("myclientid", "").
//		Build()
//
// Note the empty client secret.
func (b *TransportWrapperBuilder) Client(id string, secret string) *TransportWrapperBuilder {
	b.clientID = id
	b.clientSecret = secret
	return b
}

// User sets the user name and password that will be used to request OpenID access tokens. When
// these two values are provided the round trippers will use the resource owner password grant type
// to obtain the token. For example:
//
//	// Use the resource owner password grant:
//	wrapper, err := authentication.NewTransportWrapper().
//		User("myuser", "mypassword").
//		Build()
//
// Note that some OpenID providers (Keycloak, for example) require the client identifier also for
// the resource owner password grant. In that case use the set only the identifier, and let the
// secret blank. For example:
//
//	// Use the resource owner password grant:
//	wrapper, err := authentication.NewConnectionBuilder().
//		User("myuser", "mypassword").
//		Client("myclientid", "").
//		Build()
//
// Note the empty client secret.
func (b *TransportWrapperBuilder) User(name string, password string) *TransportWrapperBuilder {
	b.user = name
	b.password = password
	return b
}

// Scopes sets the OpenID scopes that will be included in the token request. The default is to use
// the `openid` scope. If this method is used then that default will be completely replaced, so you
// will need to specify it explicitly if you want to use it. For example, if you want to add the
// scope 'myscope' without loosing the default you will have to do something like this:
//
//	// Create a wrapper with the default 'openid' scope and some additional scopes:
//	wrapper, err := authentication.NewTransportWrapper().
//		User("myuser", "mypassword").
//		Scopes("openid", "myscope", "yourscope").
//		Build()
//
// If you just want to use the default 'openid' then there is no need to use this method.
func (b *TransportWrapperBuilder) Scopes(values ...string) *TransportWrapperBuilder {
	b.scopes = make([]string, len(values))
	copy(b.scopes, values)
	return b
}

// Tokens sets the OpenID tokens that will be used to authenticate. Multiple types of tokens are
// accepted, and used according to their type. For example, you can pass a single access token, or
// an access token and a refresh token, or just a refresh token. If no token is provided then the
// round trippers will the user name and password or the client identifier and client secret (see
// the User and Client methods) to request new ones.
//
// If the wrapper is created with these tokens and no user or client credentials, it will stop
// working when both tokens expire. That can happen, for example, if the connection isn't used for a
// period of time longer than the life of the refresh token.
func (b *TransportWrapperBuilder) Tokens(tokens ...string) *TransportWrapperBuilder {
	b.tokens = append(b.tokens, tokens...)
	return b
}

// Agent sets the `User-Agent` header that the round trippers will use in all the HTTP requests. The
// default is `OCM-SDK` followed by an slash and the version of the SDK, for example `OCM/0.0.0`.
func (b *TransportWrapperBuilder) Agent(agent string) *TransportWrapperBuilder {
	b.agent = agent
	return b
}

// TrustedCA sets a source that contains he certificate authorities that will be trusted by the HTTP
// client used to request tokens. If this isn't explicitly specified then the clients will trust the
// certificate authorities trusted by default by the system. The value can be a *x509.CertPool or a
// string, anything else will cause an error when Build method is called. If it is a *x509.CertPool
// then the value will replace any other source given before. If it is a string then it should be
// the name of a PEM file. The contents of that file will be added to the previously given sources.
func (b *TransportWrapperBuilder) TrustedCA(value interface{}) *TransportWrapperBuilder {
	if value != nil {
		b.trustedCAs = append(b.trustedCAs, value)
	}
	return b
}

// TrustedCAs sets a list of sources that contains he certificate authorities that will be trusted
// by the HTTP client used to request tokens. See the documentation of the TrustedCA method for more
// information about the accepted values.
func (b *TransportWrapperBuilder) TrustedCAs(values ...interface{}) *TransportWrapperBuilder {
	for _, value := range values {
		b.TrustedCA(value)
	}
	return b
}

// Insecure enables insecure communication with the OpenID server. This disables verification of TLS
// certificates and host names and it isn't recommended for a production environment.
func (b *TransportWrapperBuilder) Insecure(flag bool) *TransportWrapperBuilder {
	b.insecure = flag
	return b
}

// TransportWrapper adds a function that will be used to wrap the transports of the HTTP client used
// to request tokens. If used multiple times the transport wrappers will be called in the same order
// that they are added.
func (b *TransportWrapperBuilder) TransportWrapper(
	value func(http.RoundTripper) http.RoundTripper) *TransportWrapperBuilder {
	if value != nil {
		b.transportWrappers = append(b.transportWrappers, value)
	}
	return b
}

// TransportWrappers adds a list of functions that will be used to wrap the transports of the HTTP
// client used to request tokens
func (b *TransportWrapperBuilder) TransportWrappers(
	values ...func(http.RoundTripper) http.RoundTripper) *TransportWrapperBuilder {
	for _, value := range values {
		b.TransportWrapper(value)
	}
	return b
}

// MetricsSubsystem sets the name of the subsystem that will be used by the wrapper to register
// metrics with Prometheus. If this isn't explicitly specified, or if it is an empty string, then no
// metrics will be registered. For example, if the value is `api_outbound` then the following
// metrics will be registered:
//
//	api_outbound_token_request_count - Number of token requests sent.
//	api_outbound_token_request_duration_sum - Total time to send token requests, in seconds.
//	api_outbound_token_request_duration_count - Total number of token requests measured.
//	api_outbound_token_request_duration_bucket - Number of token requests organized in buckets.
//
// The duration buckets metrics contain an `le` label that indicates the upper bound. For example if
// the `le` label is `1` then the value will be the number of requests that were processed in less
// than one second.
//
//      code - HTTP response code, for example 200 or 500.
//
// The value of the `code` label will be zero when sending the request failed without a response
// code, for example if it wasn't possible to open the connection, or if there was a timeout waiting
// for the response.
//
// Note that setting this attribute is not enough to have metrics published, you also need to
// create and start a metrics server, as described in the documentation of the Prometheus library.
func (b *TransportWrapperBuilder) MetricsSubsystem(value string) *TransportWrapperBuilder {
	b.metricsSubsystem = value
	return b
}

// MetricsRegisterer sets the Prometheus registerer that will be used to register the metrics. The
// default is to use the default Prometheus registerer and there is usually no need to change that.
// This is intended for unit tests, where it is convenient to have a registerer that doesn't
// interfere with the rest of the system.
func (b *TransportWrapperBuilder) MetricsRegisterer(
	value prometheus.Registerer) *TransportWrapperBuilder {
	if value == nil {
		value = prometheus.DefaultRegisterer
	}
	b.metricsRegisterer = value
	return b
}

// Build uses the information stored in the builder to create a new transport wrapper.
func (b *TransportWrapperBuilder) Build(ctx context.Context) (result *TransportWrapper, err error) {
	// Check parameters:
	if b.logger == nil {
		err = fmt.Errorf("logger is mandatory")
		return
	}

	// Check that we have some kind of credentials or a token:
	haveTokens := len(b.tokens) > 0
	havePassword := b.user != "" && b.password != ""
	haveSecret := b.clientID != "" && b.clientSecret != ""
	if !haveTokens && !havePassword && !haveSecret {
		err = fmt.Errorf(
			"either a token, an user name and password or a client identifier and secret are " +
				"necessary, but none has been provided",
		)
		return
	}

	// Parse the tokens:
	tokenParser := new(jwt.Parser)
	var accessToken *jwt.Token
	var refreshToken *jwt.Token
	for i, text := range b.tokens {
		var token *jwt.Token
		token, _, err = tokenParser.ParseUnverified(text, jwt.MapClaims{})
		if err != nil {
			err = fmt.Errorf("can't parse token %d: %w", i, err)
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			err = fmt.Errorf("claims of token %d are of type '%T'", i, claims)
			return
		}
		claim, ok := claims["typ"]
		if !ok {
			err = fmt.Errorf("token %d doesn't contain the 'typ' claim", i)
			return
		}
		typ, ok := claim.(string)
		if !ok {
			err = fmt.Errorf("claim 'type' of token %d is of type '%T'", i, claim)
			return
		}
		switch {
		case strings.EqualFold(typ, "Bearer"):
			accessToken = token
		case strings.EqualFold(typ, "Refresh"):
			refreshToken = token
		case strings.EqualFold(typ, "Offline"):
			refreshToken = token
		default:
			err = fmt.Errorf("type '%s' of token %d is unknown", typ, i)
			return
		}
	}

	// Set the default authentication details, if needed:
	tokenURL := b.tokenURL
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
		b.logger.Debug(
			ctx,
			"Token URL wasn't provided, will use the default '%s'",
			tokenURL,
		)
	}
	tokenServer, err := internal.ParseServerAddress(ctx, tokenURL)
	if err != nil {
		err = fmt.Errorf("can't parse token URL '%s': %w", tokenURL, err)
		return
	}
	clientID := b.clientID
	if clientID == "" {
		clientID = DefaultClientID
		b.logger.Debug(
			ctx,
			"Client identifier wasn't provided, will use the default '%s'",
			clientID,
		)
	}
	clientSecret := b.clientSecret
	if clientSecret == "" {
		clientSecret = DefaultClientSecret
		b.logger.Debug(
			ctx,
			"Client secret wasn't provided, will use the default",
		)
	}

	// Set the default authentication scopes, if needed:
	scopes := b.scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	} else {
		scopes = make([]string, len(b.scopes))
		for i := range b.scopes {
			scopes[i] = b.scopes[i]
		}
	}

	// Create the client selector:
	clientSelector, err := internal.NewClientSelector().
		Logger(b.logger).
		TrustedCAs(b.trustedCAs...).
		Insecure(b.insecure).
		TransportWrappers(b.transportWrappers...).
		Build(ctx)
	if err != nil {
		return
	}

	// Register the metrics:
	var tokenCountMetric *prometheus.CounterVec
	var tokenDurationMetric *prometheus.HistogramVec
	if b.metricsSubsystem != "" {
		tokenCountMetric = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: b.metricsSubsystem,
				Name:      "token_request_count",
				Help:      "Number of token requests sent.",
			},
			tokenMetricsLabels,
		)
		err = b.metricsRegisterer.Register(tokenCountMetric)
		if err != nil {
			registered, ok := err.(prometheus.AlreadyRegisteredError)
			if ok {
				tokenCountMetric = registered.ExistingCollector.(*prometheus.CounterVec)
				err = nil
			} else {
				return
			}
		}

		tokenDurationMetric = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Subsystem: b.metricsSubsystem,
				Name:      "token_request_duration",
				Help:      "Token request duration in seconds.",
				Buckets: []float64{
					0.1,
					1.0,
					10.0,
					30.0,
				},
			},
			tokenMetricsLabels,
		)
		err = b.metricsRegisterer.Register(tokenDurationMetric)
		if err != nil {
			registered, ok := err.(prometheus.AlreadyRegisteredError)
			if ok {
				tokenDurationMetric = registered.ExistingCollector.(*prometheus.HistogramVec)
				err = nil
			} else {
				return
			}
		}
	}

	// Create and populate the object:
	result = &TransportWrapper{
		logger:              b.logger,
		clientID:            clientID,
		clientSecret:        clientSecret,
		user:                b.user,
		password:            b.password,
		scopes:              scopes,
		agent:               b.agent,
		clientSelector:      clientSelector,
		tokenURL:            tokenURL,
		tokenServer:         tokenServer,
		tokenMutex:          &sync.Mutex{},
		tokenParser:         tokenParser,
		accessToken:         accessToken,
		refreshToken:        refreshToken,
		metricsSubsystem:    b.metricsSubsystem,
		metricsRegisterer:   b.metricsRegisterer,
		tokenCountMetric:    tokenCountMetric,
		tokenDurationMetric: tokenDurationMetric,
	}

	return
}

// Logger returns the logger that is used by the wrapper.
func (w *TransportWrapper) Logger() logging.Logger {
	return w.logger
}

// TokenURL returns the URL that the connection is using request OpenID access tokens.
func (w *TransportWrapper) TokenURL() string {
	return w.tokenURL
}

// Client returns OpenID client identifier and secret that the wrapper is using to request OpenID
// access tokens.
func (w *TransportWrapper) Client() (id, secret string) {
	id = w.clientID
	secret = w.clientSecret
	return
}

// User returns the user name and password that the wrapper is using to request OpenID access
// tokens.
func (w *TransportWrapper) User() (user, password string) {
	user = w.user
	password = w.password
	return
}

// Scopes returns the OpenID scopes that the wrapper is using to request OpenID access tokens.
func (w *TransportWrapper) Scopes() []string {
	result := make([]string, len(w.scopes))
	copy(result, w.scopes)
	return result
}

// Wrap creates a new round tripper that wraps the given one and populates the authorization header.
func (w *TransportWrapper) Wrap(transport http.RoundTripper) http.RoundTripper {
	return &roundTripper{
		owner:     w,
		logger:    w.logger,
		transport: transport,
	}
}

// Close releases all the resources used by the wrapper.
func (w *TransportWrapper) Close() error {
	err := w.clientSelector.Close()
	if err != nil {
		return err
	}
	return nil
}

// RoundTrip is the implementation of the round tripper interface.
func (t *roundTripper) RoundTrip(request *http.Request) (response *http.Response, err error) {
	// Get the context:
	ctx := request.Context()

	// Get the access token:
	token, _, err := t.owner.Tokens(ctx)
	if err != nil {
		err = fmt.Errorf("can't get access token: %w", err)
		return
	}

	// Add the authorization header:
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	// Call the wrapped transport:
	response, err = t.transport.RoundTrip(request)

	return
}

// Tokens returns the access and refresh tokens that are currently in use by the wrapper. If it is
// necessary to request new tokens because they weren't requested yet, or because they are expired,
// this method will do it and will return an error if it fails.
//
// If new tokens are needed the request will be retried with an exponential backoff.
func (w *TransportWrapper) Tokens(ctx context.Context, expiresIn ...time.Duration) (access,
	refresh string, err error) {
	expiresDuration := tokenExpiry
	if len(expiresIn) == 1 {
		expiresDuration = expiresIn[0]
	}

	// Configure the back-off so that it honours the deadline of the context passed
	// to the method. Note that we need to specify explicitly the type of the variable
	// because the backoff.NewExponentialBackOff function returns the implementation
	// type but backoff.WithContext returns the interface instead.
	exponentialBackoffMethod := backoff.NewExponentialBackOff()
	exponentialBackoffMethod.MaxElapsedTime = 15 * time.Second
	var backoffMethod backoff.BackOff = exponentialBackoffMethod
	if ctx != nil {
		backoffMethod = backoff.WithContext(backoffMethod, ctx)
	}

	attempt := 0
	operation := func() error {
		attempt++
		var code int
		code, access, refresh, err = w.tokens(ctx, attempt, expiresDuration)
		if err != nil {
			if code >= http.StatusInternalServerError {
				w.logger.Error(
					ctx,
					"Can't to get tokens, got HTTP code %d, will retry: %v",
					code, err,
				)
				return err
			}
			w.logger.Error(
				ctx,
				"Can't get tokens, got HTTP code %d, will not retry: %v",
				code, err,
			)
			return backoff.Permanent(err)
		}

		if attempt > 1 {
			w.logger.Info(ctx, "Got tokens on attempt %d", attempt)
		} else {
			w.logger.Debug(ctx, "Got tokens on first attempt")
		}
		return nil
	}

	// nolint
	backoff.Retry(operation, backoffMethod)
	return access, refresh, err
}

func (w *TransportWrapper) tokens(ctx context.Context, attempt int,
	expiresIn time.Duration) (code int, access, refresh string, err error) {
	// We need to make sure that this method isn't execute concurrently, as we will be updating
	// multiple attributes of the connection:
	w.tokenMutex.Lock()
	defer w.tokenMutex.Unlock()

	// Check the expiration times of the tokens:
	now := time.Now()
	var accessExpires bool
	var accessLeft time.Duration
	if w.accessToken != nil {
		accessExpires, accessLeft, err = GetTokenExpiry(w.accessToken, now)
		if err != nil {
			return
		}
	}
	var refreshExpires bool
	var refreshLeft time.Duration
	if w.refreshToken != nil {
		refreshExpires, refreshLeft, err = GetTokenExpiry(w.refreshToken, now)
		if err != nil {
			return
		}
	}
	if w.logger.DebugEnabled() {
		w.debugExpiry(ctx, "Bearer", w.accessToken, accessExpires, accessLeft)
		w.debugExpiry(ctx, "Refresh", w.refreshToken, refreshExpires, refreshLeft)
	}

	// If the access token is available and it isn't expired or about to expire then we can
	// return the current tokens directly:
	if w.accessToken != nil && (!accessExpires || accessLeft >= expiresIn) {
		access, refresh = w.currentTokens()
		return
	}

	// At this point we know that the access token is unavailable, expired or about to expire.
	w.logger.Debug(ctx, "Trying to get new tokens (attempt %d)", attempt)

	// So we need to check if we can use the refresh token to request a new one.
	if w.refreshToken != nil && (!refreshExpires || refreshLeft >= expiresIn) {
		code, _, err = w.sendRefreshTokenForm(ctx, attempt)
		if err != nil {
			return
		}
		access, refresh = w.currentTokens()
		return
	}

	// Now we know that both the access and refresh tokens are unavailable, expired or about to
	// expire. So we need to check if we have other credentials that can be used to request a
	// new token, and use them.
	if w.haveCredentials() {
		code, _, err = w.sendRequestTokenForm(ctx, attempt)
		if err != nil {
			return
		}
		access, refresh = w.currentTokens()
		return
	}

	// Here we know that the access and refresh tokens are unavailable, expired or about to
	// expire. We also know that we don't have credentials to request new ones. But we could
	// still use the refresh token if it isn't completely expired.
	if w.refreshToken != nil && refreshLeft > 0 {
		w.logger.Warn(
			ctx,
			"Refresh token expires in only %s, but there is no other mechanism to "+
				"obtain a new token, so will try to use it anyhow",
			refreshLeft,
		)
		code, _, err = w.sendRefreshTokenForm(ctx, attempt)
		if err != nil {
			return
		}
		access, refresh = w.currentTokens()
		return
	}

	// At this point we know that the access token is expired or about to expire. We know also
	// that the refresh token is unavailable or completely expired. And we know that we don't
	// have credentials to request new tokens. But we can still use the access token if it isn't
	// expired.
	if w.accessToken != nil && accessLeft > 0 {
		w.logger.Warn(
			ctx,
			"Access token expires in only %s, but there is no other mechanism to "+
				"obtain a new token, so will try to use it anyhow",
			accessLeft,
		)
		access, refresh = w.currentTokens()
		return
	}

	// There is no way to get a valid access token, so all we can do is report the failure:
	err = fmt.Errorf(
		"access and refresh tokens are unavailable or expired, and there are no " +
			"password or client secret to request new ones",
	)

	return
}

// currentTokens returns the current tokens without trying to send any request to refresh them, and
// checking that they are actually available. If they aren't available then it will return empty
// strings.
func (w *TransportWrapper) currentTokens() (access, refresh string) {
	if w.accessToken != nil {
		access = w.accessToken.Raw
	}
	if w.refreshToken != nil {
		refresh = w.refreshToken.Raw
	}
	return
}

func (w *TransportWrapper) sendRequestTokenForm(ctx context.Context, attempt int) (code int,
	result *internal.TokenResponse, err error) {
	form := url.Values{}
	if w.havePassword() {
		w.logger.Debug(ctx, "Requesting new token using the password grant")
		form.Set("grant_type", "password")
		form.Set("client_id", w.clientID)
		form.Set("username", w.user)
		form.Set("password", w.password)
	} else if w.haveSecret() {
		w.logger.Debug(ctx, "Requesting new token using the client credentials grant")
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", w.clientID)
		form.Set("client_secret", w.clientSecret)
	} else {
		err = fmt.Errorf(
			"either password or client secret must be provided",
		)
		return
	}
	form.Set("scope", strings.Join(w.scopes, " "))
	return w.sendTokenForm(ctx, form, attempt)
}

func (w *TransportWrapper) sendRefreshTokenForm(ctx context.Context, attempt int) (code int,
	result *internal.TokenResponse, err error) {
	// Send the refresh token grant form:
	w.logger.Debug(ctx, "Requesting new token using the refresh token grant")
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", w.clientID)
	form.Set("client_secret", w.clientSecret)
	form.Set("refresh_token", w.refreshToken.Raw)
	code, result, err = w.sendTokenForm(ctx, form, attempt)

	// If the server returns an 'invalid_grant' error response then it may be that the
	// session has expired even if the tokens have not expired. This may happen when the SSO
	// server has been restarted or its session caches have been cleared. In theory that should
	// not happen, but in practice it happens from time to time, specially when using the client
	// credentials grant. To handle that smoothly we request new tokens if we have credentials
	// to do so.
	if err != nil && result != nil {
		var errorCode string
		if result.Error != nil {
			errorCode = *result.Error
		}
		var errorDescription string
		if result.ErrorDescription != nil {
			errorDescription = *result.ErrorDescription
		}
		if errorCode == "invalid_grant" && w.haveCredentials() {
			w.logger.Info(
				ctx,
				"Server returned error code '%s' and error description '%s' "+
					"when the refresh token isn't expired",
				errorCode, errorDescription,
			)
			return w.sendRequestTokenForm(ctx, attempt)
		}
	}

	return
}

func (w *TransportWrapper) sendTokenForm(ctx context.Context, form url.Values,
	attempt int) (code int, result *internal.TokenResponse, err error) {
	// Measure the time that it takes to send the request and receive the response:
	start := time.Now()
	code, result, err = w.sendTokenFormTimed(ctx, form)
	elapsed := time.Since(start)

	// Update the metrics:
	if w.tokenCountMetric != nil || w.tokenDurationMetric != nil {
		labels := map[string]string{
			metricsAttemptLabel: strconv.Itoa(attempt),
			metricsCodeLabel:    strconv.Itoa(code),
		}
		if w.tokenCountMetric != nil {
			w.tokenCountMetric.With(labels).Inc()
		}
		if w.tokenDurationMetric != nil {
			w.tokenDurationMetric.With(labels).Observe(elapsed.Seconds())
		}
	}

	// Return the original error:
	return
}

func (w *TransportWrapper) sendTokenFormTimed(ctx context.Context, form url.Values) (code int,
	result *internal.TokenResponse, err error) {
	// Create the HTTP request:
	body := []byte(form.Encode())
	request, err := http.NewRequest(http.MethodPost, w.tokenURL, bytes.NewReader(body))
	request.Close = true
	header := request.Header
	if w.agent != "" {
		header.Set("User-Agent", w.agent)
	}
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	header.Set("Accept", "application/json")
	if err != nil {
		err = fmt.Errorf("can't create request: %w", err)
		return
	}

	// Set the context:
	if ctx != nil {
		request = request.WithContext(ctx)
	}

	// Select the HTTP client:
	client, err := w.clientSelector.Select(ctx, w.tokenServer)
	if err != nil {
		return
	}

	// Send the HTTP request:
	response, err := client.Do(request)
	if err != nil {
		err = fmt.Errorf("can't send request: %w", err)
		return
	}
	defer response.Body.Close()

	code = response.StatusCode

	// Check that the response content type is JSON:
	err = internal.CheckContentType(response)
	if err != nil {
		return
	}

	// Read the response body:
	body, err = ioutil.ReadAll(response.Body)
	if err != nil {
		err = fmt.Errorf("can't read response: %w", err)
		return
	}

	// Parse the response body:
	result = &internal.TokenResponse{}
	err = json.Unmarshal(body, result)
	if err != nil {
		err = fmt.Errorf("can't parse JSON response: %w", err)
		return
	}
	if result.Error != nil {
		if result.ErrorDescription != nil {
			err = fmt.Errorf("%s: %s", *result.Error, *result.ErrorDescription)
			return
		}
		err = fmt.Errorf("%s", *result.Error)
		return
	}
	if response.StatusCode != http.StatusOK {
		err = fmt.Errorf("token response status code is '%d'", response.StatusCode)
		return
	}
	if result.TokenType != nil && *result.TokenType != "bearer" {
		err = fmt.Errorf("expected 'bearer' token type but got '%s", *result.TokenType)
		return
	}
	if result.AccessToken == nil {
		err = fmt.Errorf("no access token was received")
		return
	}
	accessToken, _, err := w.tokenParser.ParseUnverified(*result.AccessToken, jwt.MapClaims{})
	if err != nil {
		return
	}
	if result.RefreshToken == nil {
		err = fmt.Errorf("no refresh token was received")
		return
	}
	refreshToken, _, err := w.tokenParser.ParseUnverified(*result.RefreshToken, jwt.MapClaims{})
	if err != nil {
		return
	}

	// Save the new tokens:
	w.accessToken = accessToken
	w.refreshToken = refreshToken

	return
}

// haveCredentials returns true if the connection has credentials that can be used to request new
// tokens.
func (w *TransportWrapper) haveCredentials() bool {
	return w.havePassword() || w.haveSecret()
}

func (w *TransportWrapper) havePassword() bool {
	return w.user != "" && w.password != ""
}

func (w *TransportWrapper) haveSecret() bool {
	return w.clientID != "" && w.clientSecret != ""
}

// debugExpiry sends to the log information about the expiration of the given token.
func (w *TransportWrapper) debugExpiry(ctx context.Context, typ string, token *jwt.Token, expires bool,
	left time.Duration) {
	if token != nil {
		if expires {
			if left < 0 {
				w.logger.Debug(ctx, "%s token expired %s ago", typ, -left)
			} else if left > 0 {
				w.logger.Debug(ctx, "%s token expires in %s", typ, left)
			} else {
				w.logger.Debug(ctx, "%s token expired just now", typ)
			}
		}
	} else {
		w.logger.Debug(ctx, "%s token isn't available", typ)
	}
}

// GetTokenExpiry determines if the given token expires, and the time that remains till it expires.
func GetTokenExpiry(token *jwt.Token, now time.Time) (expires bool,
	left time.Duration, err error) {
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		err = fmt.Errorf("expected map claims bug got %T", claims)
		return
	}
	var exp float64
	claim, ok := claims["exp"]
	if ok {
		exp, ok = claim.(float64)
		if !ok {
			err = fmt.Errorf("expected floating point 'exp' but got %T", claim)
			return
		}
	}
	if exp == 0 {
		expires = false
		left = 0
	} else {
		expires = true
		left = time.Unix(int64(exp), 0).Sub(now)
	}
	return
}

const (
	tokenExpiry = 1 * time.Minute
)

// Names of the labels added to metrics:
const (
	metricsAttemptLabel = "attempt"
	metricsCodeLabel    = "code"
)

// Array of labels added to token metrics:
var tokenMetricsLabels = []string{
	metricsAttemptLabel,
	metricsCodeLabel,
}
