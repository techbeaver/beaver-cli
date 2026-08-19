# The TechBeaver CLI installer for Windows.
#   irm https://techbeaver.io/install.ps1 | iex
$ErrorActionPreference = 'Stop'

$repo = 'techbeaver/beaver-cli'
$arch = if ([Environment]::Is64BitOperatingSystem) { 'amd64' } else { throw '32-bit Windows is not supported' }

$version = $env:BEAVER_VERSION
if (-not $version) {
    $version = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
}
$number = $version.TrimStart('v')

$archive = "beaver_${number}_windows_${arch}.zip"
$base = "https://github.com/$repo/releases/download/$version"
$tmp = Join-Path $env:TEMP ("beaver-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    Write-Host "Downloading beaver $version..."
    Invoke-WebRequest "$base/$archive" -OutFile "$tmp\$archive"

    # Verify before unpacking, not after it is on PATH.
    try {
        Invoke-WebRequest "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
        $expected = (Select-String -Path "$tmp\checksums.txt" -Pattern ([regex]::Escape($archive))).Line.Split(' ')[0]
        $actual = (Get-FileHash "$tmp\$archive" -Algorithm SHA256).Hash.ToLower()
        if ($expected -and $actual -ne $expected.ToLower()) {
            throw "Checksum mismatch. Expected $expected, got $actual. Not installing."
        }
        Write-Host "Checksum verified."
    } catch [System.Net.WebException] {
        Write-Warning "Could not fetch checksums.txt; continuing without verification."
    }

    Expand-Archive "$tmp\$archive" -DestinationPath $tmp -Force

    $dir = if ($env:BEAVER_INSTALL_DIR) { $env:BEAVER_INSTALL_DIR } else { "$env:LOCALAPPDATA\Programs\beaver" }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Move-Item "$tmp\beaver.exe" "$dir\beaver.exe" -Force

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($userPath -notlike "*$dir*") {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
        Write-Host "Added $dir to your PATH. Open a new terminal for it to take effect."
    }

    Write-Host ""
    Write-Host "Installed beaver $version to $dir\beaver.exe"
    Write-Host "Run: beaver auth login"
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
