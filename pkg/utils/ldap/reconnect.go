// Copyright 2022 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package ldap

// LDAP automatic reconnection mechanism, inspired by:
// https://gist.github.com/emsearcy/cba3295d1a06d4c432ab4f6173b65e4f#file-ldap_snippet-go

import (
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/rs/zerolog"
)

var (
	defaultRetries = 1
	errMaxRetries  = errors.New("max retries")
)

type ldapConnection struct {
	Conn  *ldap.Conn
	Error error
}

// ConnWithReconnect maintains an LDAP Connection that automatically reconnects after network errors
type ConnWithReconnect struct {
	conn    chan ldapConnection
	reset   chan *ldap.Conn
	retries int
	logger  *zerolog.Logger
}

// Config holds the basic configuration of the LDAP Connection
type Config struct {
	URI          string
	BindDN       string
	BindPassword string
	TLSConfig    *tls.Config
}

// NewLDAPWithReconnect Returns a new ConnWithReconnect initialized from config
func NewLDAPWithReconnect(config Config) *ConnWithReconnect {
	conn := ConnWithReconnect{
		conn:    make(chan ldapConnection),
		reset:   make(chan *ldap.Conn),
		retries: defaultRetries,
	}
	logger := zerolog.Nop()
	conn.logger = &logger
	go conn.ldapAutoConnect(config)
	return &conn
}

// SetLogger sets the logger for the current instance
func (c *ConnWithReconnect) SetLogger(logger *zerolog.Logger) {
	c.logger = logger
}

func (c *ConnWithReconnect) retry(fn func(c ldap.Client) error) error {
	conn, err := c.getConnection()

	if err != nil {
		return err
	}

	for try := 0; try <= c.retries; try++ {
		if try > 0 {
			c.logger.Debug().Msgf("retrying attempt %d", try)
			conn, err = c.reconnect(conn)
			if err != nil {
				// reconnection failed stop this attempt
				return err
			}
		}
		if err = fn(conn); err == nil {
			// function succeed no need to retry
			return nil
		}
		if !ldap.IsErrorWithCode(err, ldap.ErrorNetwork) {
			// non network error, stop retrying
			return err
		}
	}
	return ldap.NewError(ldap.ErrorNetwork, errMaxRetries)
}

// Search implements the ldap.Client interface
func (c *ConnWithReconnect) Search(sr *ldap.SearchRequest) (*ldap.SearchResult, error) {
	var err error
	var res *ldap.SearchResult

	retryErr := c.retry(func(c ldap.Client) error {
		res, err = c.Search(sr)
		return err
	})

	return res, retryErr

}

// Add implements the ldap.Client interface
func (c *ConnWithReconnect) Add(a *ldap.AddRequest) error {
	err := c.retry(func(c ldap.Client) error {
		return c.Add(a)
	})

	return err
}

// Del implements the ldap.Client interface
func (c *ConnWithReconnect) Del(d *ldap.DelRequest) error {
	err := c.retry(func(c ldap.Client) error {
		return c.Del(d)
	})

	return err
}

// Modify implements the ldap.Client interface
func (c *ConnWithReconnect) Modify(m *ldap.ModifyRequest) error {
	err := c.retry(func(c ldap.Client) error {
		return c.Modify(m)
	})

	return err
}

// ModifyDN implements the ldap.Client interface
func (c *ConnWithReconnect) ModifyDN(m *ldap.ModifyDNRequest) error {
	err := c.retry(func(c ldap.Client) error {
		return c.ModifyDN(m)
	})

	return err
}

func (c *ConnWithReconnect) getConnection() (*ldap.Conn, error) {
	conn := <-c.conn
	if conn.Conn != nil && !ldap.IsErrorWithCode(conn.Error, ldap.ErrorNetwork) {
		c.logger.Debug().Msg("using existing Connection")
		return conn.Conn, conn.Error
	}
	return c.reconnect(conn.Conn)
}

func (c *ConnWithReconnect) ldapAutoConnect(config Config) {
	l, err := c.ldapConnect(config)
	if err != nil {
		c.logger.Debug().Err(err).Msg("autoconnect could not get ldap Connection")
	}

	for {
		select {
		case resConn := <-c.reset:
			// Only close the connection and reconnect if the current
			// connection, matches the one we got via the reset channel.
			// If they differ we already reconnected
			switch {
			case l == nil:
				c.logger.Debug().Msg("reconnecting to LDAP")
				l, err = c.ldapConnect(config)
			case l != resConn:
				c.logger.Debug().Msg("already reconnected")
				continue
			default:
				c.logger.Debug().Msg("closing and reconnecting to LDAP")
				l.Close()
				l, err = c.ldapConnect(config)
			}
		case c.conn <- ldapConnection{l, err}:
		}
	}
}

func (c *ConnWithReconnect) ldapConnect(config Config) (*ldap.Conn, error) {
	c.logger.Debug().Msgf("Connecting to %s", config.URI)

	var err error
	var l *ldap.Conn
	if config.TLSConfig != nil {
		l, err = ldap.DialURL(config.URI, ldap.DialWithTLSConfig(config.TLSConfig))
	} else {
		l, err = ldap.DialURL(config.URI)
	}

	if err != nil {
		c.logger.Debug().Err(err).Msg("could not get ldap Connection")
		return nil, err
	}
	c.logger.Debug().Msg("LDAP Connected")
	if config.BindDN != "" {
		c.logger.Debug().Msgf("Binding as %s", config.BindDN)
		err = l.Bind(config.BindDN, config.BindPassword)
		if err != nil {
			c.logger.Debug().Err(err).Msg("Bind failed")
			l.Close()
			return nil, err
		}

	}
	return l, err

}

func (c *ConnWithReconnect) reconnect(resetConn *ldap.Conn) (*ldap.Conn, error) {
	c.logger.Debug().Msg("LDAP connection reset")
	c.reset <- resetConn
	c.logger.Debug().Msg("Waiting for new connection")
	result := <-c.conn
	return result.Conn, result.Error
}

// Remaining methods to fulfill ldap.Client interface

// Start implements the ldap.Client interface
func (c *ConnWithReconnect) Start() {}

// StartTLS implements the ldap.Client interface
func (c *ConnWithReconnect) StartTLS(*tls.Config) error {
	return ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// Close implements the ldap.Client interface
func (c *ConnWithReconnect) Close() {}

// IsClosing implements the ldap.Client interface
func (c *ConnWithReconnect) IsClosing() bool {
	return false
}

// SetTimeout implements the ldap.Client interface
func (c *ConnWithReconnect) SetTimeout(time.Duration) {}

// Bind implements the ldap.Client interface
func (c *ConnWithReconnect) Bind(username, password string) error {
	return ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// UnauthenticatedBind implements the ldap.Client interface
func (c *ConnWithReconnect) UnauthenticatedBind(username string) error {
	return ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// SimpleBind implements the ldap.Client interface
func (c *ConnWithReconnect) SimpleBind(*ldap.SimpleBindRequest) (*ldap.SimpleBindResult, error) {
	return nil, ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// ExternalBind implements the ldap.Client interface
func (c *ConnWithReconnect) ExternalBind() error {
	return ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// ModifyWithResult implements the ldap.Client interface
func (c *ConnWithReconnect) ModifyWithResult(m *ldap.ModifyRequest) (*ldap.ModifyResult, error) {
	return nil, ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// Compare implements the ldap.Client interface
func (c *ConnWithReconnect) Compare(dn, attribute, value string) (bool, error) {
	return false, ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// PasswordModify implements the ldap.Client interface
func (c *ConnWithReconnect) PasswordModify(*ldap.PasswordModifyRequest) (*ldap.PasswordModifyResult, error) {
	return nil, ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}

// SearchWithPaging implements the ldap.Client interface
func (c *ConnWithReconnect) SearchWithPaging(searchRequest *ldap.SearchRequest, pagingSize uint32) (*ldap.SearchResult, error) {
	return nil, ldap.NewError(ldap.LDAPResultNotSupported, fmt.Errorf("not implemented"))
}
