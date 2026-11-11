# Install Borq from install.borq.io

After deploying this Worker, install the latest stable release with:

```sh
curl -fsSL https://install.borq.io | sh
```

Select an exact published tag, including a prerelease, with:

```sh
curl -fsSL 'https://install.borq.io?version=v0.2.2-rc.4' | sh
```

Without `version`, the Worker uses GitHub's latest release endpoint, which
excludes drafts and prereleases.
An unknown tag or an unavailable stable release returns an error.

The script downloads the matching archive and `checksums.txt` from
`stefanmcshane/borq` GitHub Releases. It verifies the SHA-256 checksum before
replacing the installed binary. Current release packages support Linux AMD64
and macOS Apple Silicon. Other platforms produce an error.

The default destination is `$HOME/.local/bin`. Add that directory to your
`PATH`, or set `BORQ_INSTALL_DIR` on the shell running the installer:

```sh
curl -fsSL https://install.borq.io | BORQ_INSTALL_DIR=/usr/local/bin sh
```

The destination must be writable. The script does not prompt or invoke sudo.
For CI, pin the version and add the destination to the job's `PATH`.

## Deploy the Worker

Use Node.js 22.12 or newer and a Cloudflare account that owns the `borq.io` zone.
Run these commands from the repository root:

```sh
npm ci --prefix zarf/install-worker
make test-install-worker
npm run build --prefix zarf/install-worker
npm run deploy --prefix zarf/install-worker
```

Wrangler uses your Cloudflare login, or `CLOUDFLARE_API_TOKEN` and
`CLOUDFLARE_ACCOUNT_ID` from the deployment environment. The configuration binds
the Worker to `install.borq.io`. The public Worker needs no GitHub credential.

GitHub release lookups and successful installer responses are cached for five
minutes. Both the archive and checksum URLs use the same resolved release tag,
so a newly published release cannot mix versions during an installation.

For local development, run `npm run dev --prefix zarf/install-worker`. This
serves the Worker and its installer asset locally without deploying them.
