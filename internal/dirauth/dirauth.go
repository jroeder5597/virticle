// Package dirauth signs users in against an LDAP directory or Active
// Directory: a service account searches for the user, then the user's own
// password is checked by binding as the entry that was found. Groups are read
// from memberOf and from a group search.
package dirauth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// Defaults for the settings a user may leave empty.
const (
	DefaultUserFilter  = "(|(uid={user})(sAMAccountName={user})(mail={user}))"
	DefaultGroupFilter = "(|(member={dn})(uniqueMember={dn})(memberUid={user}))"
	DefaultMemberOf    = "memberOf"
	DefaultTimeout     = 5 * time.Second

	// adCapability is in the rootDSE's supportedCapabilities on Active Directory only.
	adCapability = "1.2.840.113556.1.4.800"
	// adInChain is the matching rule that follows nested group membership.
	adInChain = "1.2.840.113556.1.4.1941"
)

// Config is how to reach and search a directory.
type Config struct {
	URL          string // ldaps://host or ldap://host
	BindDN       string // service account; empty searches anonymously
	BindPassword string
	BaseDN       string
	UserFilter   string // {user} is the typed name
	GroupFilter  string // {dn} and {user}
	NameAttr     string // attribute holding the sign-in name; empty tries uid, sAMAccountName, mail
	MemberOfAttr string
	CACert       string // PEM; empty uses the system roots
	AllowPlain   bool   // ldap:// without TLS
	Timeout      time.Duration
}

// Result is a user found and checked in the directory.
type Result struct {
	DN      string
	Name    string   // sign-in name from the directory
	Display string   // full name, may be empty
	Groups  []string // DNs and plain names of the groups, for matching
	AD      bool
}

// Errors the caller reacts to differently.
var (
	// ErrBadCredentials covers an unknown user and a wrong password alike.
	ErrBadCredentials = errors.New("wrong username or password")
	ErrAmbiguous      = errors.New("more than one directory entry matches that name")
)

// UnreachableError means the directory could not be used, not that the user is wrong.
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string { return e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

func expand(filter, user, dn string) string {
	r := strings.NewReplacer("{user}", ldap.EscapeFilter(user), "{dn}", ldap.EscapeFilter(dn))
	return r.Replace(filter)
}

// Validate checks the settings that can be checked without a network.
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
		return errors.New("the server address must look like ldaps://dc.example.org")
	}
	if c.BaseDN == "" {
		return errors.New("the base DN is required")
	}
	if c.UserFilter != "" && !strings.Contains(c.UserFilter, "{user}") {
		return errors.New("the user filter must contain {user}")
	}
	if c.CACert != "" {
		if _, err := pool(c.CACert); err != nil {
			return err
		}
	}
	return nil
}

func pool(pem string) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM([]byte(pem)) {
		return nil, errors.New("the CA certificate is not a valid PEM certificate")
	}
	return p, nil
}

func (c Config) dial(note func(bool, string)) (*ldap.Conn, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if c.CACert != "" {
		if tlsCfg.RootCAs, err = pool(c.CACert); err != nil {
			return nil, err
		}
	}
	dialer := &net.Dialer{Timeout: c.timeout()}
	conn, err := ldap.DialURL(c.URL, ldap.DialWithDialer(dialer), ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, unreachable(err)
	}
	conn.SetTimeout(c.timeout())
	switch {
	case u.Scheme == "ldaps":
		note(true, "Connected over LDAPS and the certificate is trusted.")
	case c.AllowPlain:
		note(true, "Connected without encryption (you allowed plain LDAP).")
	default:
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, unreachable(fmt.Errorf("StartTLS failed: %w", err))
		}
		note(true, "Connected and switched to an encrypted connection (StartTLS); the certificate is trusted.")
	}
	return conn, nil
}

func unreachable(err error) error {
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	msg := err.Error()
	switch {
	case errors.As(err, &ua), errors.As(err, &ci), strings.Contains(msg, "unknown authority"), strings.Contains(msg, "certificate has expired"):
		err = errors.New("the directory's certificate is not trusted; paste the directory's CA certificate under Advanced")
	case errors.As(err, &hn), strings.Contains(msg, "certificate is valid for"):
		err = errors.New("the directory's certificate does not match the server name in the address")
	}
	return &UnreachableError{err}
}

// Authenticate finds user, checks password and reads the groups.
func Authenticate(cfg Config, user, password string) (Result, error) {
	if password == "" {
		return Result{}, ErrBadCredentials // an empty password would be an anonymous bind, which many servers accept
	}
	return run(cfg, user, password, func(bool, string) {})
}

// Step is one line of a test report.
type Step struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// Test walks the same steps as a sign-in and reports each one. user may be
// empty (only connect and bind), password may be empty (do not check it).
func Test(cfg Config, user, password string) []Step {
	var steps []Step
	note := func(ok bool, text string) { steps = append(steps, Step{ok, text}) }
	if err := cfg.Validate(); err != nil {
		return []Step{{false, err.Error()}}
	}
	res, err := run(cfg, user, password, note)
	if err != nil {
		text := err.Error()
		if errors.Is(err, ErrBadCredentials) && password == "" {
			text = "That user was not found."
		}
		note(false, text)
		return steps
	}
	_ = res
	return steps
}

func run(cfg Config, user, password string, note func(bool, string)) (Result, error) {
	var res Result
	test := note
	if err := cfg.Validate(); err != nil {
		return res, err
	}
	conn, err := cfg.dial(note)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	bindService := func() error {
		if cfg.BindDN == "" {
			return conn.UnauthenticatedBind("")
		}
		return conn.Bind(cfg.BindDN, cfg.BindPassword)
	}
	if err := bindService(); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return res, &UnreachableError{errors.New("the directory refused the bind user's name or password")}
		}
		return res, unreachable(err)
	}
	test(true, "Bound as the bind user.")
	if user == "" {
		return res, nil
	}
	filter := cfg.UserFilter
	if filter == "" {
		filter = DefaultUserFilter
	}
	memberOf := cfg.MemberOfAttr
	if memberOf == "" {
		memberOf = DefaultMemberOf
	}
	attrs := []string{"dn", "uid", "sAMAccountName", "mail", "cn", "displayName", memberOf}
	if cfg.NameAttr != "" {
		attrs = append(attrs, cfg.NameAttr)
	}
	sr, err := conn.Search(ldap.NewSearchRequest(cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(cfg.timeout().Seconds()), false,
		expand(filter, user, ""), attrs, nil))
	if err != nil {
		return res, &UnreachableError{fmt.Errorf("the user search failed: %w", err)}
	}
	switch len(sr.Entries) {
	case 0:
		return res, ErrBadCredentials
	case 1:
	default:
		return res, ErrAmbiguous
	}
	e := sr.Entries[0]
	res.DN = e.DN
	res.Name = pickName(e, cfg.NameAttr, user)
	res.Display = e.GetAttributeValue("displayName")
	if res.Display == "" {
		res.Display = e.GetAttributeValue("cn")
	}
	test(true, fmt.Sprintf("User found as %s.", e.DN))
	if password != "" {
		if err := conn.Bind(e.DN, password); err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
				return res, ErrBadCredentials
			}
			return res, &UnreachableError{err}
		}
		test(true, "The password is correct.")
		if err := bindService(); err != nil {
			return res, unreachable(err)
		}
	}
	groups := map[string]bool{}
	var list, names []string
	add := func(dn string) {
		if groups[strings.ToLower(dn)] {
			return
		}
		for _, g := range []string{dn, cnOf(dn)} {
			if g != "" {
				groups[strings.ToLower(g)] = true
				list = append(list, g)
			}
		}
		if n := cnOf(dn); n != "" {
			names = append(names, n)
		} else {
			names = append(names, dn)
		}
	}
	for _, g := range e.GetAttributeValues(memberOf) {
		add(g)
	}
	// Active Directory is recognised by the rootDSE, and then nested groups
	// are followed with the in-chain matching rule.
	gBase, gFilter := cfg.BaseDN, cfg.GroupFilter
	if rs, err := conn.Search(ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false, "(objectClass=*)",
		[]string{"supportedCapabilities", "defaultNamingContext"}, nil)); err == nil && len(rs.Entries) == 1 {
		for _, c := range rs.Entries[0].GetAttributeValues("supportedCapabilities") {
			res.AD = res.AD || c == adCapability
		}
		if res.AD {
			if d := rs.Entries[0].GetAttributeValue("defaultNamingContext"); d != "" {
				gBase = d
			}
			if gFilter == "" || gFilter == DefaultGroupFilter {
				gFilter = "(&(objectClass=group)(member:" + adInChain + ":={dn}))"
			}
		}
	}
	if gFilter == "" {
		gFilter = DefaultGroupFilter
	}
	if gs, err := conn.Search(ldap.NewSearchRequest(gBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, int(cfg.timeout().Seconds()), false,
		expand(gFilter, res.Name, e.DN), []string{"dn", "cn"}, nil)); err == nil {
		for _, g := range gs.Entries {
			add(g.DN)
		}
	} else {
		test(false, "The group search failed: "+err.Error())
	}
	res.Groups = list
	kind := "an LDAP directory"
	if res.AD {
		kind = "Active Directory (nested groups are followed)"
	}
	test(true, fmt.Sprintf("The server is %s. The user is in %d group(s)%s.", kind, len(names), groupsText(names)))
	return res, nil
}

func groupsText(names []string) string {
	if len(names) == 0 {
		return ""
	}
	if len(names) > 8 {
		names = append(append([]string{}, names[:8]...), "and more")
	}
	return ": " + strings.Join(names, ", ")
}

func pickName(e *ldap.Entry, attr, typed string) string {
	try := []string{attr, "uid", "sAMAccountName", "mail"}
	for _, a := range try {
		if a != "" {
			if v := e.GetAttributeValue(a); v != "" {
				return v
			}
		}
	}
	return typed
}

// cnOf returns the first RDN value of a DN ("CN=Admins,OU=x" gives "Admins"),
// or "" when s is not a DN.
func cnOf(dn string) string {
	if !strings.Contains(dn, "=") {
		return ""
	}
	first, _, _ := strings.Cut(dn, ",")
	_, v, ok := strings.Cut(first, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// InGroup reports whether the result lists want (a group DN or name, any case).
func (r Result) InGroup(want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	for _, g := range r.Groups {
		if strings.ToLower(g) == want {
			return true
		}
	}
	return false
}
