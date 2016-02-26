/*
Copyright 2015 Gravitational, Inc.

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

package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"github.com/mailgun/ttlmap"
	"golang.org/x/crypto/ssh"
)

// sessionContext is a context associated with users'
// web session, it stores connected client that persists
// between requests for example to avoid connecting
// to the auth server on every page hit
type sessionContext struct {
	*log.Entry
	sess   *auth.Session
	user   string
	clt    *auth.TunClient
	parent *sessionCache
}

func (c *sessionContext) Invalidate() error {
	return c.parent.InvalidateSession(c)
}

// GetClient returns the client connected to the auth server
func (c *sessionContext) GetClient() (auth.ClientI, error) {
	return c.clt, nil
}

// GetUser returns the authenticated teleport user
func (c *sessionContext) GetUser() string {
	return c.user
}

// GetWebSession returns a web session
func (c *sessionContext) GetWebSession() *auth.Session {
	return c.sess
}

// CreateNewWebSession creates a new web session for this user
// based on the previous session
func (c *sessionContext) CreateWebSession() (*auth.Session, error) {
	sess, err := c.clt.CreateWebSession(c.user, c.sess.ID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return sess, nil
}

// GetAuthMethods returns authentication methods (credentials) that proxy
// can use to connect to servers
func (c *sessionContext) GetAuthMethods() ([]ssh.AuthMethod, error) {
	a, err := c.clt.GetAgent()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	signers, err := a.Signers()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

// Close cleans up connections associated with requests
func (c *sessionContext) Close() error {
	if c.clt != nil {
		return trace.Wrap(c.clt.Close())
	}
	return nil
}

// newSessionHandler returns new instance of the session handler
func newSessionHandler(secure bool, servers []utils.NetAddr) (*sessionCache, error) {
	m, err := ttlmap.NewMap(1024, ttlmap.CallOnExpire(closeContext))
	if err != nil {
		return nil, err
	}
	return &sessionCache{
		contexts:    m,
		authServers: servers,
	}, nil
}

// sessionCache handles web session authentication,
// and holds in memory contexts associated with each session
type sessionCache struct {
	sync.Mutex
	secure      bool
	contexts    *ttlmap.TtlMap
	authServers []utils.NetAddr
}

// closeContext is called when session context expires from
// cache and will clean up connections
func closeContext(key string, val interface{}) {
	log.Infof("closing context %v", key)
	ctx := val.(*sessionContext)
	if err := ctx.Close(); err != nil {
		log.Infof("failed to close context: %v", err)
	}
}

func (s *sessionCache) Auth(user, pass string, hotpToken string) (*auth.Session, error) {
	method, err := auth.NewWebPasswordAuth(user, []byte(pass), hotpToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], user, method)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return clt.SignIn(user, []byte(pass))
}

func (s *sessionCache) GetCertificate(c createSSHCertReq) (*SSHLoginResponse, error) {
	method, err := auth.NewWebPasswordAuth(c.User, []byte(c.Password),
		c.HOTPToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	clt, err := auth.NewTunClient(s.authServers[0], c.User, method)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cert, err := clt.GenerateUserCert(c.PubKey, c.User, c.TTL)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	hostSigners, err := clt.GetCertAuthorities(services.HostCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	signers := []services.CertAuthority{}
	for _, hs := range hostSigners {
		signers = append(signers, *hs)
	}

	return &SSHLoginResponse{
		Cert:        cert,
		HostSigners: signers,
	}, nil
}

func (s *sessionCache) GetUserInviteInfo(token string) (user string,
	QRImg []byte, hotpFirstValues []string, e error) {

	method, err := auth.NewSignupTokenAuth(token)
	if err != nil {
		return "", nil, nil, trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], "tokenAuth", method)
	if err != nil {
		return "", nil, nil, trace.Wrap(err)
	}

	return clt.GetSignupTokenData(token)
}

func (s *sessionCache) CreateNewUser(token, password, hotpToken string) (*auth.Session, error) {
	method, err := auth.NewSignupTokenAuth(token)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], "tokenAuth", method)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := clt.CreateUserWithToken(token, password, hotpToken)
	return sess, trace.Wrap(err)
}

func (s *sessionCache) InvalidateSession(ctx *sessionContext) error {
	defer ctx.Close()
	if err := s.resetContext(ctx.GetUser(), ctx.GetWebSession().ID); err != nil {
		return trace.Wrap(err)
	}
	clt, err := ctx.GetClient()
	if err != nil {
		return trace.Wrap(err)
	}
	err = clt.DeleteWebSession(ctx.GetUser(), ctx.GetWebSession().ID)
	return trace.Wrap(err)
}

func (s *sessionCache) getContext(user, sid string) (*sessionContext, error) {
	s.Lock()
	defer s.Unlock()

	val, ok := s.contexts.Get(user + sid)
	if ok {
		return val.(*sessionContext), nil
	}
	return nil, trace.Wrap(teleport.NotFound("sessionContext not found"))
}

func (s *sessionCache) insertContext(user, sid string, ctx *sessionContext, ttl time.Duration) (*sessionContext, error) {
	s.Lock()
	defer s.Unlock()

	val, ok := s.contexts.Get(user + sid)
	if ok && val != nil { // nil means that we've just invalidated the context now and set it to nil in the cache
		return val.(*sessionContext), trace.Wrap(&teleport.AlreadyExistsError{})
	}
	if err := s.contexts.Set(user+sid, ctx, int(ttl/time.Second)); err != nil {
		return nil, trace.Wrap(err)
	}
	return ctx, nil
}

func (s *sessionCache) resetContext(user, sid string) error {
	s.Lock()
	defer s.Unlock()
	return trace.Wrap(s.contexts.Set(user+sid, nil, 1))
}

func (s *sessionCache) ValidateSession(user, sid string) (*sessionContext, error) {
	ctx, err := s.getContext(user, sid)
	if err == nil {
		ctx.Infof("got from cache")
		return ctx, nil
	}
	method, err := auth.NewWebSessionAuth(user, []byte(sid))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], user, method)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := clt.GetWebSessionInfo(user, sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	c := &sessionContext{
		clt:    clt,
		user:   user,
		sess:   sess,
		parent: s,
	}
	c.Entry = log.WithFields(log.Fields{
		"user": user,
		"sess": sess.ID[:4],
	})
	out, err := s.insertContext(user, sid, c, auth.WebSessionTTL)
	if err != nil {
		// this means that someone has just inserted the context, so
		// close our extra context and return
		if teleport.IsAlreadyExists(err) {
			ctx.Infof("just created, returning the existing one")
			defer c.Close()
			return out, nil
		}
		return nil, trace.Wrap(err)
	}
	return out, nil
}

func (s *sessionCache) SetSession(w http.ResponseWriter, user, sid string) error {
	d, err := EncodeCookie(user, sid)
	if err != nil {
		return err
	}
	c := &http.Cookie{
		Name:     "session",
		Value:    d,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
	}
	http.SetCookie(w, c)
	return nil
}

func (s *sessionCache) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
	})
}
