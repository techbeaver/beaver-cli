# The TechBeaver CLI installer for Windows.
#   irm https://techbeaver.io/install.ps1 | iex
#
# Fails closed: a missing or mismatching checksum stops the install. When cosign
# is on PATH the signature on checksums.txt is verified against this repository's
# release workflow. Without cosign the installer says so and continues on the
# checksum alone, unless BEAVER_REQUIRE_SIGNATURE=1 is set, which makes a missing
# cosign fatal.
$ErrorActionPreference = 'Stop'

$repo = 'techbeaver/beaver-cli'
# The exact release workflow on the exact tag being installed, and GitHub Actions as the issuer.
$signerWorkflow = 'https://github.com/techbeaver/beaver-cli/.github/workflows/release.yml@refs/tags/'
$signerIssuer = 'https://token.actions.githubusercontent.com'

$arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { throw '32-bit Windows is not supported' }

$version = $env:BEAVER_VERSION
if (-not $version) {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
}
if (-not $version.StartsWith('v')) { $version = "v$version" }
# The version goes into a URL and a file name, so it is checked rather than trusted.
if ($version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') {
    throw "That is not a release version: $version"
}
$number = $version.Substring(1)

$archive = "beaver_${number}_windows_${arch}.zip"
$base = "https://github.com/$repo/releases/download/$version"
$tmp = Join-Path $env:TEMP ("beaver-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    Write-Host "Downloading beaver $version..."
    Invoke-WebRequest "$base/$archive" -OutFile "$tmp\$archive"
    try {
        Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
    } catch {
        throw "Could not download checksums.txt, so the download cannot be verified. Not installing."
    }

    $cosign = Get-Command cosign -ErrorAction SilentlyContinue
    if ($cosign) {
        try {
            Invoke-WebRequest "$base/checksums.txt.sig" -OutFile "$tmp\checksums.txt.sig"
            Invoke-WebRequest "$base/checksums.txt.pem" -OutFile "$tmp\checksums.txt.pem"
        } catch {
            throw "Could not download the signature for checksums.txt. Not installing."
        }
        & $cosign.Source verify-blob `
            --certificate "$tmp\checksums.txt.pem" `
            --signature "$tmp\checksums.txt.sig" `
            --certificate-identity "$signerWorkflow$version" `
            --certificate-oidc-issuer $signerIssuer `
            "$tmp\checksums.txt" | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "The signature on checksums.txt did not verify against this repository's release workflow. Not installing."
        }
        Write-Host "Signature verified."
    } elseif ($env:BEAVER_REQUIRE_SIGNATURE -eq '1') {
        throw "BEAVER_REQUIRE_SIGNATURE=1 is set but cosign is not installed, so the release signature cannot be verified."
    } else {
        Write-Warning "cosign is not installed, so the release signature was NOT verified."
        Write-Warning "The checksum proves the download is intact. It does not prove who built it."
        Write-Warning "Install cosign to check the signature, or set BEAVER_REQUIRE_SIGNATURE=1 to refuse to continue without it."
    }

    # Verify before unpacking, not after it is on PATH. A missing entry, a
    # duplicate entry or a malformed hash is a failure, never a reason to continue.
    $entries = @(Get-Content "$tmp\checksums.txt" | ForEach-Object {
        $parts = $_ -split '\s+', 2
        if ($parts.Count -eq 2 -and ($parts[1].TrimStart('*') -eq $archive)) { $parts[0] }
    })
    if ($entries.Count -ne 1 -or $entries[0] -notmatch '^[0-9a-f]{64}$') {
        throw "checksums.txt does not list $archive exactly once with a valid SHA-256. Not installing."
    }
    $actual = (Get-FileHash "$tmp\$archive" -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $entries[0]) {
        throw "Checksum mismatch. Expected $($entries[0]), got $actual. Not installing."
    }
    Write-Host "Checksum verified."

    Expand-Archive "$tmp\$archive" -DestinationPath $tmp -Force

    $dir = if ($env:BEAVER_INSTALL_DIR) { $env:BEAVER_INSTALL_DIR } else { "$env:LOCALAPPDATA\Programs\beaver" }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Move-Item "$tmp\beaver.exe" "$dir\beaver.exe" -Force

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = @($userPath -split ';' | Where-Object { $_ }) }
    $present = $entries | Where-Object { $_.TrimEnd('\') -ieq $dir.TrimEnd('\') }
    if (-not $present) {
        $newPath = if ($userPath) { $userPath.TrimEnd(';') + ";$dir" } else { $dir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Host "Added $dir to your PATH. Open a new terminal for it to take effect."
    }

    Write-Host ""
    Write-Host "Installed beaver $version to $dir\beaver.exe"
    Write-Host "Run: beaver auth login"
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
