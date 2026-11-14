#!/usr/bin/env bash
# Builds the static package repository (apt and dnf) that is published on
# GitHub Pages. Stateless: everything is rebuilt from the packages given.
#
#   build-repo.sh PACKAGE_DIR OUT_DIR
#
# PACKAGE_DIR  holds the virticle*.deb and virticle*.rpm files (all versions).
# OUT_DIR      is created; its content is the whole site.
#
# Needs: bash, gpg, apt-ftparchive (apt-utils), createrepo_c, rpmsign (rpm),
# gzip. The signing key must already be in the current GNUPGHOME.
#
# Environment (all optional):
#   BASE_URL             where the site will be served, no trailing slash
#                        (default https://jacobanthonyroeder.github.io/virticle)
#   GPG_KEY              fingerprint of the signing key (default: the only
#                        secret key; it is an error if there are several)
#   GPG_PASSPHRASE_FILE  file holding the key passphrase; leave unset when the
#                        key has none
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 PACKAGE_DIR OUT_DIR" >&2
  exit 2
fi
pkgdir=$(cd "$1" && pwd)
out=$2
BASE_URL=${BASE_URL:-https://jacobanthonyroeder.github.io/virticle}
BASE_URL=${BASE_URL%/}

for t in gpg apt-ftparchive createrepo_c rpmsign gzip; do
  command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }
done

shopt -s nullglob
debs=("$pkgdir"/*.deb)
rpms=("$pkgdir"/*.rpm)
if [ ${#debs[@]} -eq 0 ] || [ ${#rpms[@]} -eq 0 ]; then
  echo "no .deb or no .rpm files in $pkgdir" >&2
  exit 1
fi

# Signing key.
if [ -z "${GPG_KEY:-}" ]; then
  mapfile -t fprs < <(gpg --batch --list-secret-keys --with-colons | awk -F: '$1=="sec"{s=1;next} $1=="fpr"&&s{print $10;s=0}')
  if [ ${#fprs[@]} -ne 1 ]; then
    echo "expected exactly one secret key in GNUPGHOME, found ${#fprs[@]}; set GPG_KEY" >&2
    exit 1
  fi
  GPG_KEY=${fprs[0]}
fi

gpgopts=(--batch --yes --local-user "$GPG_KEY")
if [ -n "${GPG_PASSPHRASE_FILE:-}" ]; then
  gpgopts+=(--pinentry-mode loopback --passphrase-file "$GPG_PASSPHRASE_FILE")
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
[ -z "$(ls -A "$out")" ] || { echo "$out is not empty" >&2; exit 1; }

# Public key.
gpg --batch --yes --armor --export "$GPG_KEY" > "$out/virticle.asc"
gpg --batch --yes --export "$GPG_KEY" > "$out/virticle.gpg"
[ -s "$out/virticle.gpg" ] || { echo "could not export the public key" >&2; exit 1; }

# ---- apt: pool/ and dists/stable/main/binary-ARCH ----
apt="$out/apt"
pool="$apt/pool/main/v/virticle"
mkdir -p "$pool"
cp "${debs[@]}" "$pool/"
archs="amd64 arm64"
(
  cd "$apt"
  for a in $archs; do
    d=dists/stable/main/binary-$a
    mkdir -p "$d"
    apt-ftparchive --arch "$a" packages pool > "$d/Packages"
    gzip -9nk "$d/Packages"
  done
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=Virticle \
    -o APT::FTPArchive::Release::Label=Virticle \
    -o APT::FTPArchive::Release::Suite=stable \
    -o APT::FTPArchive::Release::Codename=stable \
    -o APT::FTPArchive::Release::Architectures="$archs" \
    -o APT::FTPArchive::Release::Components=main \
    release dists/stable > Release.new
  mv Release.new dists/stable/Release
  gpg "${gpgopts[@]}" --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
  gpg "${gpgopts[@]}" --clearsign -o dists/stable/InRelease dists/stable/Release
)

# ---- rpm: one repository for every RPM distribution and both architectures ----
rpm="$out/rpm"
mkdir -p "$rpm"
cp "${rpms[@]}" "$rpm/"

# rpmsign runs gpg through a macro; build it so that the optional passphrase
# works without a prompt.
signcmd="%{__gpg} gpg --no-verbose --no-armor --batch --yes --no-secmem-warning"
if [ -n "${GPG_PASSPHRASE_FILE:-}" ]; then
  signcmd="$signcmd --pinentry-mode loopback --passphrase-file $GPG_PASSPHRASE_FILE"
fi
signcmd="$signcmd --digest-algo sha256 -u %{_gpg_name} -sbo %{__signature_filename} %{__plaintext_filename}"
rpmsign --define "_gpg_name $GPG_KEY" --define "__gpg_sign_cmd $signcmd" --addsign "$rpm"/*.rpm >/dev/null

createrepo_c --quiet --no-database "$rpm"
gpg "${gpgopts[@]}" --armor --detach-sign -o "$rpm/repodata/repomd.xml.asc" "$rpm/repodata/repomd.xml"

# ---- client files ----
cat > "$out/virticle.repo" <<EOF
[virticle]
name=Virticle
baseurl=$BASE_URL/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$BASE_URL/virticle.asc
EOF

fpr=$(gpg --batch --with-colons --fingerprint "$GPG_KEY" | awk -F: '$1=="fpr"{print $10; exit}')
cat > "$out/index.html" <<EOF
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Virticle packages</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem}pre{overflow-x:auto;background:#f2f2f2;color:#111;padding:.75rem}@media(prefers-color-scheme:dark){body{background:#111;color:#eee}pre{background:#222;color:#eee}}</style>
</head>
<body>
<h1>Virticle packages</h1>
<p>Signed apt and dnf repository. Key fingerprint: <code>$fpr</code></p>
<h2>Debian, Ubuntu</h2>
<pre>sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL $BASE_URL/virticle.gpg -o /etc/apt/keyrings/virticle.gpg
echo "deb [signed-by=/etc/apt/keyrings/virticle.gpg] $BASE_URL/apt stable main" | sudo tee /etc/apt/sources.list.d/virticle.list
sudo apt update
sudo apt install virticle</pre>
<h2>Fedora, Rocky, Alma, RHEL</h2>
<pre>sudo curl -fsSL $BASE_URL/virticle.repo -o /etc/yum.repos.d/virticle.repo
sudo dnf install virticle</pre>
<p>Then run <code>sudo virticle setup</code>. Files: <a href="virticle.asc">virticle.asc</a>, <a href="virticle.gpg">virticle.gpg</a>, <a href="virticle.repo">virticle.repo</a>.</p>
</body>
</html>
EOF

echo "repository built in $out (key $fpr)"
