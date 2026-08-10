[CmdletBinding()]
param(
    [string]$Version = "0.02",
    [string]$NdkPath = "",
    [string]$PythonPath = "",
    [long]$SourceDateEpoch = 1785340800,
    [switch]$SkipTests,
    [switch]$Skip7Zip
)

$ErrorActionPreference = "Stop"

function Assert-NativeSuccess {
    param([string]$Step, [int]$ExitCode)
    if ($ExitCode -ne 0) {
        throw "$Step failed with exit code $ExitCode."
    }
}

$Repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Build = Join-Path $Repo "build"
$Dist = Join-Path $Repo "dist"
$ModuleStage = Join-Path $Build "module"
$Go = Join-Path $Repo ".tools\go\bin\go.exe"
if (-not (Test-Path -LiteralPath $Go)) {
    $GoCommand = Get-Command go -ErrorAction SilentlyContinue
    if ($null -eq $GoCommand) {
        throw "Go 1.26.5 is required. Install it or extract it to .tools\go."
    }
    $Go = $GoCommand.Source
}
if ([string]::IsNullOrWhiteSpace($PythonPath)) {
    $PythonCommand = Get-Command python -ErrorAction SilentlyContinue
    if ($null -eq $PythonCommand) {
        $PythonCommand = Get-Command python3 -ErrorAction SilentlyContinue
    }
    if ($null -eq $PythonCommand) {
        throw "Python 3.11+ is required for ELF and ZIP verification; use -PythonPath to select it."
    }
    $PythonPath = $PythonCommand.Source
}

if ([string]::IsNullOrWhiteSpace($NdkPath)) {
    if (-not [string]::IsNullOrWhiteSpace($env:ANDROID_NDK_HOME)) {
        $NdkPath = $env:ANDROID_NDK_HOME
    } elseif (-not [string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
        $NdkPath = Join-Path $env:LOCALAPPDATA "Android\Sdk\ndk\29.0.14206865"
    }
}
if (-not (Test-Path -LiteralPath $NdkPath)) {
    throw "Android NDK r29 was not found: $NdkPath"
}

$MappedDrive = $null
if ($NdkPath -match "[^\u0000-\u007F]") {
    foreach ($letter in @("N", "M", "L", "K")) {
        if (-not (Test-Path "$letter`:\")) {
            subst "$letter`:" $NdkPath
            $MappedDrive = "$letter`:"
            $NdkPath = "$letter`:\"
            break
        }
    }
    if ($null -eq $MappedDrive) {
        throw "The NDK path contains non-ASCII characters and no drive letter is available for subst."
    }
}

try {
    New-Item -ItemType Directory -Force -Path $Build, $Dist | Out-Null
    $env:SOURCE_DATE_EPOCH = [string]$SourceDateEpoch
    $env:GOTOOLCHAIN = "local"
    if ([string]::IsNullOrWhiteSpace($env:GOPROXY)) {
        $env:GOPROXY = "https://proxy.golang.org,direct"
    }

    & $Go version
    Assert-NativeSuccess "Go version check" $LASTEXITCODE
    $ActualGo = & $Go env GOVERSION
    Assert-NativeSuccess "Go environment query" $LASTEXITCODE
    if ($ActualGo -ne "go1.26.5") {
        throw "The Go toolchain must be exactly 1.26.5; found $ActualGo"
    }
    $NdkProperties = Join-Path $NdkPath "source.properties"
    if (-not (Test-Path -LiteralPath $NdkProperties)) {
        throw "The NDK is missing source.properties: $NdkProperties"
    }
    $NdkRevisionLine = Select-String -LiteralPath $NdkProperties -Pattern '^\s*Pkg\.Revision\s*=\s*29\.0\.14206865\s*$'
    if ($null -eq $NdkRevisionLine) {
        throw "Android NDK must be exactly r29 (29.0.14206865)."
    }
    & $Go mod verify
    Assert-NativeSuccess "Go module verification" $LASTEXITCODE
    & $Go test ./internal/ops -run '^TestEmbeddedAppQueryHelper$' -count=1
    Assert-NativeSuccess "Embedded PackageManager helper verification" $LASTEXITCODE
    if (-not $SkipTests) {
        & $Go test ./...
        if ($LASTEXITCODE -ne 0) { throw "Unit tests failed." }
        if ([string]::IsNullOrWhiteSpace($env:CC)) {
            $RaceCompiler = Get-Command gcc -ErrorAction SilentlyContinue
            if ($null -eq $RaceCompiler) {
                throw "Windows race tests require a MinGW-w64 GCC on PATH. Install one or use -SkipTests only after CI has passed."
            }
            $env:CC = $RaceCompiler.Source
        }
        $env:CGO_ENABLED = "1"
        & $Go test -race ./...
        if ($LASTEXITCODE -ne 0) { throw "Race tests failed." }
    }
    & $Go mod vendor
    Assert-NativeSuccess "Go vendor generation" $LASTEXITCODE
    & $Go run -mod=vendor ./cmd/zcr521d schema --output (Join-Path $Repo "schemas\tools.json")
    if ($LASTEXITCODE -ne 0) {
        throw "Schema generation failed."
    }

    $Commit = "unknown"
    if ((Get-Command git -ErrorAction SilentlyContinue) -and (Test-Path -LiteralPath (Join-Path $Repo ".git"))) {
        $CommitCandidate = (& git -C $Repo rev-parse --verify HEAD 2>$null)
        if ($LASTEXITCODE -eq 0) { $Commit = $CommitCandidate.Trim() }
    }
    $BuildTime = [DateTimeOffset]::FromUnixTimeSeconds($SourceDateEpoch).UtcDateTime.ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ModulePropPath = Join-Path $Repo "module\module.prop"
    $ModulePropSHA256 = (Get-FileHash -LiteralPath $ModulePropPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $LdFlags = "-s -w -buildid= -X github.com/zcr521/android-ai-mcp/internal/buildinfo.Version=$Version -X github.com/zcr521/android-ai-mcp/internal/buildinfo.Commit=$Commit -X github.com/zcr521/android-ai-mcp/internal/buildinfo.BuildTime=$BuildTime -X github.com/zcr521/android-ai-mcp/internal/buildinfo.ModulePropSHA256=$ModulePropSHA256 -linkmode external -extldflags=-Wl,-z,max-page-size=16384"
    $AndroidOut = Join-Path $Build "android"
    if (Test-Path -LiteralPath $AndroidOut) {
        Remove-Item -LiteralPath $AndroidOut -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $AndroidOut | Out-Null
    $Toolchain = Join-Path $NdkPath "toolchains\llvm\prebuilt\windows-x86_64\bin"
    $Targets = @(
        @{ ABI="arm64-v8a"; GOARCH="arm64"; CC="aarch64-linux-android26-clang.cmd"; Machine="183" },
        @{ ABI="armeabi-v7a"; GOARCH="arm"; GOARM="7"; CC="armv7a-linux-androideabi26-clang.cmd"; Machine="40" },
        @{ ABI="x86_64"; GOARCH="amd64"; CC="x86_64-linux-android26-clang.cmd"; Machine="62" }
    )
    foreach ($target in $Targets) {
        $AbiOut = Join-Path $AndroidOut $target.ABI
        New-Item -ItemType Directory -Force -Path $AbiOut | Out-Null
        $env:GOOS = "android"
        $env:GOARCH = $target.GOARCH
        $env:GOARM = if ($target.ContainsKey("GOARM")) { $target.GOARM } else { "" }
        $env:CGO_ENABLED = "1"
        $env:CC = Join-Path $Toolchain $target.CC
        $DaemonOutput = Join-Path $AbiOut "zcr521d"
        & $Go build -mod=vendor -trimpath -buildmode=pie -tags "osusergo,netgo" -ldflags $LdFlags -o $DaemonOutput ./cmd/zcr521d
        Assert-NativeSuccess "Go Android build for $($target.ABI)" $LASTEXITCODE
        if (-not (Test-Path -LiteralPath $DaemonOutput)) {
            throw "Go Android build did not create $DaemonOutput."
        }
        & $PythonPath (Join-Path $Repo "scripts\verify_elf.py") --file $DaemonOutput --machine $target.Machine --page-size 16384 --api 26
        Assert-NativeSuccess "Go ELF verification for $($target.ABI)" $LASTEXITCODE
    }
    Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM, Env:CGO_ENABLED, Env:CC -ErrorAction SilentlyContinue

    if (-not $Skip7Zip) {
        $Bash = Get-Command bash -ErrorAction SilentlyContinue
        if ($null -eq $Bash) {
            throw "Building 7zz requires bash (Git Bash, WSL or Linux CI); placeholder binaries are forbidden."
        }
        $env:ANDROID_NDK_HOME = $NdkPath.Replace("\", "/")
        $env:ANDROID_API_LEVEL = "26"
        $env:SEVENZIP_OUT_DIR = (Join-Path $Repo "third_party\7zip\out").Replace("\", "/")
        & $Bash.Source (Join-Path $Repo "third_party\7zip\build-android.sh")
        $SevenZipExitCode = $LASTEXITCODE
        Remove-Item Env:ANDROID_API_LEVEL, Env:SEVENZIP_OUT_DIR -ErrorAction SilentlyContinue
        if ($SevenZipExitCode -ne 0) {
            throw "7-Zip Android build failed with exit code $SevenZipExitCode."
        }
    }
    foreach ($target in $Targets) {
        $SevenZipBinary = Join-Path $Repo ("third_party\7zip\out\" + $target.ABI + "\7zz")
        if (-not (Test-Path -LiteralPath $SevenZipBinary)) {
            throw "Missing 7-Zip Android artifact: $SevenZipBinary"
        }
        & $PythonPath (Join-Path $Repo "scripts\verify_elf.py") --file $SevenZipBinary --machine $target.Machine --page-size 16384 --api 26
        if ($LASTEXITCODE -ne 0) {
            throw "7-Zip ELF verification failed for $($target.ABI)."
        }
    }

    if (Test-Path -LiteralPath $ModuleStage) {
        Remove-Item -LiteralPath $ModuleStage -Recurse -Force
    }
    Copy-Item -LiteralPath (Join-Path $Repo "module") -Destination $ModuleStage -Recurse
    Copy-Item -LiteralPath (Join-Path $Repo "LICENSE") -Destination (Join-Path $ModuleStage "LICENSE")
    $LegalSections = [ordered]@{
        "THIRD_PARTY_NOTICES" = (Join-Path $Repo "THIRD_PARTY_NOTICES.md")
        "7ZIP_LICENSE" = (Join-Path $Repo "third_party\7zip\LICENSE-7ZIP.txt")
        "LGPL_2_1" = (Join-Path $Repo "third_party\7zip\src\DOC\copying.txt")
        "CODER_WEBSOCKET" = (Join-Path $Repo "vendor\github.com\coder\websocket\LICENSE.txt")
        "MODELCONTEXTPROTOCOL_GO_SDK" = (Join-Path $Repo "vendor\github.com\modelcontextprotocol\go-sdk\LICENSE")
        "ULIKUNITZ_XZ" = (Join-Path $Repo "vendor\github.com\ulikunitz\xz\LICENSE")
        "GOOGLE_JSONSCHEMA_GO" = (Join-Path $Repo "vendor\github.com\google\jsonschema-go\LICENSE")
        "SEGMENTIO_ASM" = (Join-Path $Repo "vendor\github.com\segmentio\asm\LICENSE")
        "SEGMENTIO_ENCODING" = (Join-Path $Repo "vendor\github.com\segmentio\encoding\LICENSE")
        "YOSIDA95_URITEMPLATE" = (Join-Path $Repo "vendor\github.com\yosida95\uritemplate\v3\LICENSE")
        "GOLANG_X_NET" = (Join-Path $Repo "vendor\golang.org\x\net\LICENSE")
        "GOLANG_X_OAUTH2" = (Join-Path $Repo "vendor\golang.org\x\oauth2\LICENSE")
        "GOLANG_X_SYNC" = (Join-Path $Repo "vendor\golang.org\x\sync\LICENSE")
        "GOLANG_X_SYS" = (Join-Path $Repo "vendor\golang.org\x\sys\LICENSE")
        "GOLANG_X_TEXT" = (Join-Path $Repo "vendor\golang.org\x\text\LICENSE")
        "GOLANG_X_TIME" = (Join-Path $Repo "vendor\golang.org\x\time\LICENSE")
    }
    $NoticeBuilder = New-Object System.Text.StringBuilder
    foreach ($LegalName in $LegalSections.Keys) {
        $LegalSource = $LegalSections[$LegalName]
        if (-not (Test-Path -LiteralPath $LegalSource)) {
            throw "Required legal notice is missing: $LegalSource"
        }
        $LegalText = [IO.File]::ReadAllText($LegalSource).Replace("`r`n", "`n").Replace("`r", "`n").TrimEnd("`n")
        [void]$NoticeBuilder.AppendLine("===== BEGIN $LegalName =====")
        [void]$NoticeBuilder.Append($LegalText)
        [void]$NoticeBuilder.AppendLine()
        [void]$NoticeBuilder.AppendLine("===== END $LegalName =====")
        [void]$NoticeBuilder.AppendLine()
    }
    $NoticeText = $NoticeBuilder.ToString().Replace("`r`n", "`n")
    [IO.File]::WriteAllText((Join-Path $ModuleStage "NOTICE"), $NoticeText, [Text.UTF8Encoding]::new($false))
    foreach ($target in $Targets) {
        $TargetDir = Join-Path $ModuleStage ("bin\" + $target.ABI)
        New-Item -ItemType Directory -Force -Path $TargetDir | Out-Null
        Copy-Item -LiteralPath (Join-Path $AndroidOut ($target.ABI + "\zcr521d")) -Destination (Join-Path $TargetDir "zcr521d")
        Copy-Item -LiteralPath (Join-Path $Repo ("third_party\7zip\out\" + $target.ABI + "\7zz")) -Destination (Join-Path $TargetDir "7zz")
    }

    & $PythonPath (Join-Path $Repo "scripts\package.py") --module-stage $ModuleStage --dist $Dist --version $Version --epoch $SourceDateEpoch
    if ($LASTEXITCODE -ne 0) {
        throw "Packaging failed."
    }
    & $PythonPath (Join-Path $Repo "scripts\verify_module.py") (Join-Path $Dist "ZCR521-Android-AI-MCP-v$Version-universal.zip")
    if ($LASTEXITCODE -ne 0) {
        throw "Module verification failed."
    }
} finally {
    if ($null -ne $MappedDrive) {
        subst $MappedDrive /D
    }
}
