# IBM Bob

IBM Bob is a VS Code fork; `agentop configure bob` routes it through Cortex via the VS Code proxy setting.

## Routing the IBM Bob editor through Cortex (`agentop configure bob`)

IBM Bob is a VS Code fork, so it reads the VS Code proxy setting. `agentop
configure bob enable` writes exactly one flat, top-level key into Bob's user
settings:

```json
{
  "http.proxy": "http://127.0.0.1:47600"
}
```

Flat and dotted, not nested under an `"http"` object — that is the shape VS Code
reads, and the shape difference from `configure claude-code`, which writes a
nested `"env"` block. The address is read from `listener.forward_proxy_addr` in
`~/.cortex/config.yaml` on every run, so a moved port or an IPv6 loopback
produces the right value rather than a hardcoded 47600.

Nothing else in the file changes, and that is meant literally: the key is spliced
into the existing bytes rather than re-serialized from a parsed document, so your
key order, your indent width, your inline arrays and your blank lines between
groups all survive untouched. A settings.json is hand-curated and often lives in
a dotfiles repo, where a diff that alphabetizes and reflows the whole file is
worse than the setting is worth. `disable` takes the line back out the same way,
so enable-then-disable returns the file byte-for-byte.

```sh
agentop configure bob enable      # write the key, print the CA trust command
agentop configure bob disable     # remove the key, print the optional undo
agentop configure bob status      # report, and act on nothing
```

`enable` and `disable` show the one-line change and ask before writing, keep a
`.bak` of the file as it was first found, and take `--yes` for unattended use.
Declined — or with no terminal to ask on — they write nothing and exit **3**, the
same convention as `configure claude-code` and `configure bobshell`. `--settings
PATH` and `--config PATH` override either file. Restart Bob afterwards: whether
it re-reads a proxy change live is unverified, so the message says restart rather
than guess.

### Certificate trust is printed, never performed

The proxy terminates TLS with a forged leaf, so Bob has to trust Cortex's bridge
CA or every HTTPS request fails. Installing a root CA is a machine-wide change
needing `sudo`, and a tool that silently escalates to do it is not what anyone
wants — so `enable` prints the exact command and stops:

```sh
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain ~/.cortex/ca/ca.crt
```

`disable` prints the undo, which is **two** commands rather than one, and says
it is safe to leave the certificate in place:

```sh
sudo security remove-trusted-cert -d ~/.cortex/ca/ca.crt
sudo security delete-certificate -c authbridge-tls-bridge-ca \
  -t /Library/Keychains/System.keychain
```

`delete-certificate` alone does not undo the `add-trusted-cert` above it. The add
writes trust settings to the **admin** domain (that is what its `-d` selects);
`delete-certificate -t` removes the certificate and, per its own usage text,
*user* trust settings — a different domain, so the admin-domain trust survives it.
`remove-trusted-cert -d` is the documented inverse of the add, and its `-d` has to
be repeated for the same reason. The keychain is named on the delete because an
add to the System keychain is not undone by a delete that defaults to the login
one.

On macOS, `status` suggests `security verify-cert` without running it. Off
macOS it suggests nothing: there is no portable check to name, and `enable` and
`disable` already carry the trust-store guidance for those platforms — naming
the usual Debian and Fedora routes and saying plainly that the exact step
depends on the distribution.

It is `ca.crt` — the single bridge CA — and deliberately **not** the
`bundle.crt` in the same directory, which holds the bridge CA *plus* every
platform root and exists for tools whose CA setting *replaces* the trust store
(`SSL_CERT_FILE` and friends, as `agentop exec` sets). The keychain is additive,
so `-r trustRoot` on the bundle would install explicit machine-wide root trust
for every public CA in it, and the undo above would not take that back.

### What it knows, and what it does not

"Enabled" means the value's **host and port both match** `listener.forward_proxy_addr`
from `~/.cortex/config.yaml` — the address this machine's Cortex actually listens
on, not a port range. Compared whole via `url.Parse`, so neither a path
(`http://corp.example.com/?next=127.0.0.1:47600`) nor a suffixed host
(`localhost:47600.evil.com`) can pass as ours; only `http` counts, since it is the
only scheme `enable` writes. The loopback spellings are folded together
(`localhost` / `127.0.0.1` / `::1`) because `forward_proxy_addr` may bind `0.0.0.0`
while the settings file names `127.0.0.1`, and a hand-typed address must not be
called someone else's proxy.

That is still not a record agentop keeps — there is no state file, so ownership is
re-decided from the value each time.

Ownership has three answers, not two. A value that matches is **ours**; a
corporate proxy, or a non-string value, is **not ours** and is left alone by both
verbs. The third is **cannot tell**: when there is no address to compare against
— the config is missing or unreadable — any loopback `http` proxy could be this
one, and a bool would have to guess. Guessing "not ours" toward a `delete` is the
dangerous direction, so it is not a bool. `status` reports "cannot tell" in those
words rather than ruling on it. `disable` asks before removing such a value and
refuses under `--yes`, since `--yes` means "do not ask me", not "decide for me".
`enable` refuses rather than overwriting anything it does not own. Either verb
prints exactly what it will do to the file, and what it will leave beside it,
before it does it.

**Whether anything is listening is a separate question**, reported on its own
line. A stopped Cortex is the normal state of a laptop and is not a verdict on the
setting: the setting is right either way, and the answer to "nothing is listening"
is `agentop service start`, not an edit here.

`http.proxy` governs VS Code's core networking and its extension host. An
extension that bundles its own HTTP client can still go around it; this is the
documented lever, not a guarantee of coverage.

A settings file with comments in it is refused, not rewritten. VS Code permits
them; the strict JSON reader here does not, and silently stripping a user's
comments to add one key is the wrong trade.

`enable` and `disable` need a settings document to already exist: a path that is
missing, empty, or holds only `null` means IBM Bob has not saved settings there,
and both refuse rather than creating a file at a path nothing reads. The refusal
names the path, and `--settings` if it is the wrong one. `status` reports the
Cortex status as unknown there, saying which of the three it found.

Only macOS's settings location is known (`~/Library/Application Support/IBM
Bob/User/settings.json`). Elsewhere `--settings PATH` is required rather than
guessed — writing a proxy setting into a file nothing reads is a silent no-op,
which is worse than a refusal that names the flag.

