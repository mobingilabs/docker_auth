/*
   Copyright 2015 Cesanta Software Ltd.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       https://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package authn

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/cesanta/glog"
	"github.com/go-ldap/ldap"
)

type LDAPAuthConfig struct {
	Addr                  string `mapstructure:"addr,omitempty"`
	TLS                   string `mapstructure:"tls,omitempty"`
	InsecureTLSSkipVerify bool   `mapstructure:"insecuretlsskipverify,omitempty"`
	CACertificate         string `mapstructure:"cacertificate,omitempty"`
	Base                  string `mapstructure:"base,omitempty"`
	Filter                string `mapstructure:"filter,omitempty"`
	BindDN                string `mapstructure:"binddn,omitempty"`
	BindPasswordFile      string `mapstructure:"bindpasswordfile,omitempty"`
	GroupBaseDN           string `mapstructure:"groupbasedn,omitempty"`
	GroupFilter           string `mapstructure:"groupfilter,omitempty"`
}

type LDAPAuth struct {
	config *LDAPAuthConfig
}

func NewLDAPAuth(c *LDAPAuthConfig) (*LDAPAuth, error) {
	if c.TLS == "" && strings.HasSuffix(c.Addr, ":636") {
		c.TLS = "always"
	}
	return &LDAPAuth{
		config: c,
	}, nil
}

//How to authenticate user, please refer to https://github.com/go-ldap/ldap/blob/master/example_test.go#L166
func (la *LDAPAuth) Authenticate(account string, password PasswordString) (bool, Labels, error) {
	if account == "" || password == "" {
		return false, nil, NoMatch
	}
	l, err := la.ldapConnection()
	if err != nil {
		return false, nil, err
	}
	defer l.Close()

	// First bind with a read only user, to prevent the following search won't perform any write action
	if bindErr := la.bindReadOnlyUser(l); bindErr != nil {
		return false, nil, bindErr
	}

	account = la.escapeAccountInput(account)

	filter := la.getFilter(account)
	accountEntryDN, uSearchErr := la.ldapSearch(l, &la.config.Base, &filter, &[]string{})
	if uSearchErr != nil {
		return false, nil, uSearchErr
	}
	if accountEntryDN == "" {
		return false, nil, NoMatch // User does not exist
	}
	// Bind as the user to verify their password
	if len(accountEntryDN) > 0 {
		err := l.Bind(accountEntryDN, string(password))
		if err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
				return false, nil, nil
			}
			return false, nil, err
		}
	}
	// Rebind as the read only user for any futher queries
	if bindErr := la.bindReadOnlyUser(l); bindErr != nil {
		return false, nil, bindErr
	}

	return true, nil, nil
}

func (la *LDAPAuth) bindReadOnlyUser(l *ldap.Conn) error {
	if la.config.BindDN != "" {
		password, err := ioutil.ReadFile(la.config.BindPasswordFile)
		if err != nil {
			return err
		}
		password_str := strings.TrimSpace(string(password))
		glog.V(2).Infof("Bind read-only user (DN = %s)", la.config.BindDN)
		err = l.Bind(la.config.BindDN, password_str)
		if err != nil {
			return err
		}
	}
	return nil
}

//To prevent LDAP injection, some characters must be escaped for searching
//e.g. char '\' will be replaced by hex '\5c'
//Filter meta chars are choosen based on filter complier code
//https://github.com/go-ldap/ldap/blob/master/filter.go#L159
func (la *LDAPAuth) escapeAccountInput(account string) string {
	r := strings.NewReplacer(
		`\`, `\5c`,
		`(`, `\28`,
		`)`, `\29`,
		`!`, `\21`,
		`*`, `\2a`,
		`&`, `\26`,
		`|`, `\7c`,
		`=`, `\3d`,
		`>`, `\3e`,
		`<`, `\3c`,
		`~`, `\7e`,
	)
	return r.Replace(account)
}

func (la *LDAPAuth) ldapConnection() (*ldap.Conn, error) {
	var l *ldap.Conn
	var err error

	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	if !la.config.InsecureTLSSkipVerify {
		addr := strings.Split(la.config.Addr, ":")
		if la.config.CACertificate != "" {
			pool := x509.NewCertPool()
			pem, err := ioutil.ReadFile(la.config.CACertificate)
			if err != nil {
				return nil, fmt.Errorf("Error loading CA File: %s", err)
			}
			ok := pool.AppendCertsFromPEM(pem)
			if !ok {
				return nil, fmt.Errorf("Error loading CA File: Couldn't parse PEM in: %s", la.config.CACertificate)
			}
			tlsConfig = &tls.Config{InsecureSkipVerify: false, ServerName: addr[0], RootCAs: pool}
		} else {
			tlsConfig = &tls.Config{InsecureSkipVerify: false, ServerName: addr[0]}
		}
	}

	if la.config.TLS == "" || la.config.TLS == "none" || la.config.TLS == "starttls" {
		glog.V(2).Infof("Dial: starting...%s", la.config.Addr)
		l, err = ldap.Dial("tcp", fmt.Sprintf("%s", la.config.Addr))
		if err == nil && la.config.TLS == "starttls" {
			glog.V(2).Infof("StartTLS...")
			if tlserr := l.StartTLS(tlsConfig); tlserr != nil {
				return nil, tlserr
			}
		}
	} else if la.config.TLS == "always" {
		glog.V(2).Infof("DialTLS: starting...%s", la.config.Addr)
		l, err = ldap.DialTLS("tcp", fmt.Sprintf("%s", la.config.Addr), tlsConfig)
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (la *LDAPAuth) getFilter(account string) string {
	filter := strings.NewReplacer("${account}", account).Replace(la.config.Filter)
	glog.V(2).Infof("search filter is %s", filter)
	return filter
}

//ldap search and return required attributes' value from searched entries
//default return entry's DN value if you leave attrs array empty
func (la *LDAPAuth) ldapSearch(l *ldap.Conn, baseDN *string, filter *string, attrs *[]string) (string, error) {
	if l == nil {
		return "", fmt.Errorf("No ldap connection!")
	}
	glog.V(2).Infof("Searching...basedDN:%s, filter:%s", *baseDN, *filter)
	searchRequest := ldap.NewSearchRequest(
		*baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		*filter,
		*attrs,
		nil)
	sr, err := l.Search(searchRequest)
	if err != nil {
		return "", err
	}

	if len(sr.Entries) == 0 {
		return "", nil // User does not exist
	} else if len(sr.Entries) > 1 {
		return "", fmt.Errorf("Too many entries returned.")
	}

	var buffer bytes.Buffer
	for _, entry := range sr.Entries {
		if len(*attrs) == 0 {
			glog.V(2).Infof("Entry DN = %s", entry.DN)
			buffer.WriteString(entry.DN)
		} else {
			for _, attr := range *attrs {
				values := strings.Join(entry.GetAttributeValues(attr), " ")
				glog.V(2).Infof("Entry %s = %s", attr, values)
				buffer.WriteString(values)
			}
		}
	}

	return buffer.String(), nil
}

func (la *LDAPAuth) Stop() {
}

func (la *LDAPAuth) Name() string {
	return "LDAP"
}
