# Release Checklist

When ready to cut a release for Orbit, follow these steps:

## 1. Prepare the Release

- [ ] Ensure all tests pass: `go test ./...`
- [ ] Run `gofmt -w .` and `go vet ./...` to ensure code is clean
- [ ] Update `version` in `cmd/orbit/main.go` to the new version (e.g., `0.2.0`)
- [ ] Verify `go build -o bin/orbit ./cmd/orbit` succeeds

## 2. Commit and Tag

```sh
git add -A
git commit -m "Release v0.2.0"
git tag v0.2.0
git push origin main
git push origin v0.2.0
```

GitHub Actions will automatically:
- Build binaries for arm64 and amd64
- Compute SHA256 checksums
- Create a GitHub Release with the binaries and checksums

## 3. Update the Homebrew Formula

After the GitHub Release is created and you've verified the binaries:

1. Download both binaries and compute their SHA256:
   ```sh
   curl -L https://github.com/anthropics/orbit/releases/download/v0.2.0/orbit-aarch64-apple-darwin -o orbit-arm64
   curl -L https://github.com/anthropics/orbit/releases/download/v0.2.0/orbit-x86_64-apple-darwin -o orbit-amd64
   shasum -a 256 orbit-*
   ```

2. Update `Formula/orbit.rb` with the new version and SHA256 hashes:
   ```ruby
   version "0.2.0"
   # Replace the sha256 values below
   sha256 "COPY-ACTUAL-HASH-HERE" # arm64
   sha256 "COPY-ACTUAL-HASH-HERE" # amd64
   ```

3. Test the formula locally:
   ```sh
   brew install --build-from-source ./Formula/orbit.rb
   ```

4. Push the updated formula to your Homebrew tap (e.g., `anthropics/orbit` or `rexfordmachu/orbit`):
   ```sh
   cd /path/to/homebrew-orbit  # your tap repo
   git add Formula/orbit.rb
   git commit -m "orbit: update to v0.2.0"
   git push
   ```

## 4. Announce

- [ ] Update any external documentation linking to Orbit
- [ ] Share the release notes in relevant channels

## Creating a Homebrew Tap (First Time Only)

If you haven't created a personal tap yet:

```sh
# Create a new repo on GitHub: https://github.com/machugram/homebrew-orbit

git clone git@github.com:machugram/homebrew-orbit.git
mkdir -p Formula
cp /path/to/orbit/Formula/orbit.rb Formula/
git add Formula/orbit.rb
git commit -m "release: orbit formula"
git push origin main
```

Then users can install with:
```sh
brew tap machugram/orbit
brew install orbit
```

## Notes

- The GitHub Actions workflow builds on every tag push and creates a release automatically
- Checksums are included in the release assets (`.sha256` files) for verification
- The Homebrew formula must use the exact SHA256 hashes from the GitHub release for security
- Once the formula is in homebrew-core, users won't need to `tap` manually

## Rollback

If a release has issues:

```sh
git tag -d v0.2.0
git push origin :v0.2.0  # delete remote tag
# Manually delete the GitHub Release from the web UI
```
