# Sign-in: two-step, identity provider, directory

Virticle signs people in three ways, and they can be combined:

- a **local account** (user name and password kept by Virticle), optionally with **two-step sign-in** from an authenticator app,
- an **identity provider** (OpenID Connect): Microsoft Entra ID, Google Workspace, Okta, Keycloak, Authentik, Duo Single Sign-On and others,
- a **directory** (LDAP or Active Directory): the same form as local accounts, checked against the directory.

The local administrator account always works, whatever is set up here. It is the way back in if a provider or directory is
down or wrongly set. There is no setting that hides the local form.

Settings are under **Settings, Sign-in** (administrators). Every change to them is written to the audit log (never with a secret).

## Two-step sign-in (authenticator app)

Standard time-based one-time passwords (RFC 6238: SHA-1, 6 digits, 30 seconds). Works with Google Authenticator, Microsoft
Authenticator, Duo Mobile (as a third-party account), Authy, 1Password and other authenticator apps.

**Turn it on** (each user, under My account): Turn on two-step sign-in, scan the QR code (or type the key), enter one code.
Ten one-time recovery codes are then shown once; copy or download them and keep them safe. They are stored hashed.

**Sign in**: after the correct password a second page asks for the 6-digit code, or a recovery code. The session only exists
after both steps. A code works once (the same code is refused a second time), and the previous and next 30-second code are
accepted to allow for a phone clock that is a little off. Wrong codes count toward the same rate limit as wrong passwords.

**Turn it off** needs the password and a current code. **New recovery codes** needs the same.

**Require two-step sign-in for** (Settings, Sign-in): *Nobody (each user decides)* (default), *Administrators* or *Everyone*.
Someone who must have it and has not set it up is taken to the set-up page right after signing in and can do nothing else
until it is done (this also applies to sessions that are already open). It does not apply to people who sign in through an
identity provider: the provider's own second factor applies there. Directory users can use it, and the setting applies to them.

API tokens are for machines and are not affected. Creating one needs a session that passed the second step.

**Lost phone?** An administrator opens Users and chooses **Reset two-step sign-in** for the user (the user is signed out
everywhere and can set it up again). If the only administrator is locked out, on the controller:

    virticle user reset-2fa USER
    virticle user reset-password USER        # asks for the new password on the terminal

`reset-password` refuses when stdin is not a terminal, unless you add `--password-stdin` and send the password on stdin.
Both take `-data DIR` (default `/var/lib/virticle` on an installed machine, else `./data`), edit the database directly and need
no restart. They only work for local accounts (reset-password) and end the user's sessions.

### Security notes

The seeds are stored encrypted (AES-256-GCM) with a random key in `secret.key` in the data directory (mode 0600, created on
first use). The identity provider's client secret and the directory bind password are sealed the same way. Protect the data
directory: it also holds the controller's SSH key and the database. Someone who copies only the database cannot read the seeds;
someone who can read the whole data directory can (and can already reach every node). If `secret.key` is lost, the seeds and
stored secrets cannot be read again: reset two-step sign-in for the users and re-enter the secrets.

## Identity provider (OpenID Connect)

Standard authorization code flow with PKCE. Virticle reads `<issuer>/.well-known/openid-configuration`, checks the ID token
signature (against the provider's keys, which may rotate), issuer, audience, expiry and nonce, and uses the `state` parameter.

In Settings, Sign-in, **Identity provider (OIDC)**:

| Field | What to enter |
| --- | --- |
| Allow sign-in with an identity provider | tick when ready |
| Provider name | shown on the button: "Sign in with *name*" |
| Issuer URL | see the table below; must be https (http only for localhost) |
| Client ID, Client secret | from the provider; the secret is never shown again |
| Redirect URL | shown read-only; register exactly this address at the provider (it is this controller's address as your browser sees it) |
| Group that gives the administrator role / operator role | a group name or id as it appears in the token |
| Everyone else who signs in gets | *No access* (default) or *Viewer* |
| Advanced: Groups claim, User name claim, Scopes | defaults `groups`, `preferred_username` (falls back to the e-mail address), `openid profile email` |

**Test** fetches the discovery document and the keys and says what it found. Use it before saving.

How people are handled: an account is created at the first sign-in, marked as coming from the identity provider, with no local
password. The role is worked out from the groups **at every sign-in**: remove someone from the group and they lose the role (or
are refused) at the next sign-in, and their open session ends then. Sessions of these users last at most 12 hours. The Users
page shows the source, and the role cannot be edited by hand. A name that a local account already uses is never taken over.
After signing out, if the provider advertises a sign-out address, a link offers to sign out there too.

**For push approvals (Duo, Microsoft Authenticator), sign in through your identity provider; its second factor applies. The
built-in two-step sign-in is for local accounts.**

### Values per provider

These come from each provider's documentation and are the usual values; only dex (with its test connector) was run against
Virticle here. Names of menus change from time to time.

| Provider | Issuer URL | Where to register | Groups |
| --- | --- | --- | --- |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant id>/v2.0` | App registrations, New registration, platform *Web*, redirect URL as shown; Certificates & secrets, New client secret: enter the **Value** as client secret, the Application (client) ID as client ID | Token configuration, Add groups claim (Security groups). The token holds group **object ids**: enter the object id of the group in the role fields. Users in very many groups get no group list in the token (Entra then sends a link instead); keep the groups assigned to the app small (assign only the groups that matter to the application) |
| Google Workspace | `https://accounts.google.com` | Google Cloud console, APIs & Services, Credentials, OAuth client ID, type *Web application*, authorized redirect URI as shown. Limit it to your organization on the consent screen (*Internal*) | Google sends no groups. Everyone who signs in gets the "Everyone else" role (choose *Viewer*); give higher roles with a local account, or put Keycloak or Authentik in front of Google |
| Okta | `https://<your-org>.okta.com` (or `.../oauth2/default` for the default authorization server) | Applications, Create App Integration, OIDC, *Web Application*, sign-in redirect URI as shown; assign people or groups | Add a groups claim: Sign On tab, OpenID Connect ID Token, Groups claim type *Filter*, name `groups`, filter *Matches regex* `.*`. Enter group names |
| Keycloak | `https://<host>/realms/<realm>` | Clients, Create client, *OpenID Connect*, Client authentication **on**, Standard flow on, valid redirect URI as shown; the secret is on the Credentials tab | Client scopes, create a scope `groups` with a *Group Membership* mapper (claim name `groups`, Full group path **off**), add it to the client as a default scope, and add `groups` to the Scopes field under Advanced |
| Authentik | `https://<host>/application/o/<application slug>/` | Applications, Create with provider, *OAuth2/OpenID*, confidential client, redirect URI (strict) as shown | The default `profile` scope already carries `groups` (group names). Enter group names |
| Duo Single Sign-On | the discovery address Duo shows for the application, without `/.well-known/openid-configuration` | Duo Admin Panel, Applications, Protect an Application, *Generic OIDC Relying Party* (Duo SSO), redirect URL as shown; copy client ID and secret | Duo sends groups only if you map an attribute for them; otherwise use *Viewer* as "Everyone else", or map a group attribute and name it under Advanced, Groups claim |

If the **Test** says the provider lists no `groups` claim, that is often only a missing line in the discovery document; sign in
once and look at the audit log and the role.

## Directory (LDAP or Active Directory)

People sign in on the normal form with their directory name and password. Virticle tries a local account first, then the
directory. A local account with the same name always wins. A directory that cannot be reached never blocks local accounts (a short
timeout, and the sign-in page says the directory could not be reached).

How it works: a service account (the *bind user*) searches for the person under the base DN, then Virticle checks the password
by binding as the entry that was found. Groups come from `memberOf` and from a group search. On Active Directory (recognised
from the server's root DSE) nested groups are followed with the matching-rule-in-chain search; on other servers only direct
membership is used.

**LDAPS or StartTLS is required**; the certificate is checked. If the directory uses your own CA, paste its certificate under
Advanced. Plain LDAP works only with the explicit checkbox under Advanced (passwords then cross the network unencrypted).

| Field | OpenLDAP example | Active Directory example |
| --- | --- | --- |
| Server URL | `ldaps://ldap.example.org` | `ldaps://dc1.example.org` |
| Bind user | `cn=virticle,ou=service,dc=example,dc=org` | `CN=virticle,OU=Service,DC=example,DC=org` (or `virticle@example.org`) |
| Bind password | the service account's password | the service account's password |
| Base DN | `dc=example,dc=org` | `DC=example,DC=org` |
| Group that gives the administrator role | `virtadmins` or its full DN | `Virticle Admins` or its full DN |
| Group that gives the operator role | `virtops` | `Virticle Operators` |
| Everyone else who signs in gets | No access (default) or Viewer | the same |

Advanced: *User filter* (default `(|(uid={user})(sAMAccountName={user})(mail={user}))`, `{user}` is the typed name, escaped),
*Group filter* (default `(|(member={dn})(uniqueMember={dn})(memberUid={user}))`), *User name attribute* (empty: `uid`, then
`sAMAccountName`, then `mail`), *Group membership attribute* (`memberOf`), *CA certificate*, *Timeout* (5 seconds), and
*Allow plain LDAP*.

**Test** takes a user name and, optionally, that user's password, and reports each step in plain words: connected, bound, user
found as *dn*, password correct, groups, and the role the user would get. Nothing is stored.

Directory users are created at the first sign-in, marked as coming from the directory, and get their role from the groups at
every sign-in (a user who loses the groups is refused and signed out). Sessions last at most 12 hours. They can turn on the
built-in two-step sign-in (a directory has no second factor of its own), and "Require two-step sign-in" applies to them.
Passwords are never logged, and the sign-in rate limit applies to the directory too.
