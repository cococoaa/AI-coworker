param(
    [int]$DefaultPort = 8792,
    [int]$MunicipalPort = 8793,
    [string]$StateRoot = (Join-Path ([IO.Path]::GetTempPath()) ('reimburse-demo-' + [guid]::NewGuid())),
    [switch]$Verify
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$state = (New-Item -ItemType Directory -Path $StateRoot).FullName
$data = Join-Path $state 'demo-data'
$cases = Join-Path $data 'cases'
New-Item -ItemType Directory -Path $cases | Out-Null

function Add-DemoCase($Source, $Id, $Title, $ClaimNo, $SubmitDate = '') {
    $target = Join-Path $cases $Id
    Copy-Item -LiteralPath (Join-Path $repo "data/cases/$Source") -Destination $target -Recurse
    $claimFile = Join-Path $target 'claim.json'
    $claim = Get-Content -LiteralPath $claimFile -Raw | ConvertFrom-Json
    $claim.claim_no = $ClaimNo
    if ($SubmitDate) { $claim.submit_date = $SubmitDate }
    $claim | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $claimFile -Encoding utf8
    $truthFile = Join-Path $target 'truth.json'
    $truth = Get-Content -LiteralPath $truthFile -Raw | ConvertFrom-Json
    $truth.id = $Id
    $truth.title = $Title
    $truth | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $truthFile -Encoding utf8
}

Add-DemoCase C01 D01 '制度对比 · 出差结束后第 25 天提交' DEMO-001 '2026-10-28'
Add-DemoCase C44 D02 '补件续审 · 缺铁路行程单' DEMO-002
Add-DemoCase C51 D03 '人工交接 · 发票查验超时' DEMO-003
Add-DemoCase C02 D04 '查重回写 · 首次提交' DEMO-004
Add-DemoCase C02 D05 '查重回写 · 同票再次提交' DEMO-005

$servers = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
function Start-DemoServer($Name, $Port, $Rules = '') {
    $base = "http://127.0.0.1:$Port"
    try { $null = Invoke-WebRequest "$base/api/meta" -TimeoutSec 1; throw "Port $Port is already in use" }
    catch { if ($_.Exception.Message -like 'Port *') { throw } }

    $args = @(
        '-addr', "127.0.0.1:$Port", '-data', ('"' + $data + '"'),
        '-demo-synthetic', '-replay', 'disabled-for-demo', '-upload-dir', '-',
        '-local-operator', 'E3001',
        '-review-file', ('"' + (Join-Path $state "$Name-reviews.json") + '"'),
        '-task-file', ('"' + (Join-Path $state "$Name-tasks.json") + '"'),
        '-quota-file', ('"' + (Join-Path $state "$Name-quota.json") + '"')
    )
    if ($Rules) { $args += @('-rules', $Rules) }
    $process = Start-Process -FilePath $binary -ArgumentList $args -WorkingDirectory $repo -WindowStyle Hidden -PassThru -RedirectStandardError (Join-Path $state "$Name.log")
    $servers.Add($process)
    for ($i = 0; $i -lt 80; $i++) {
        if ($process.HasExited) { throw (Get-Content -LiteralPath (Join-Path $state "$Name.log") -Raw) }
        try { $null = Invoke-RestMethod "$base/api/meta" -TimeoutSec 1; return $base }
        catch { Start-Sleep -Milliseconds 125 }
    }
    throw "Server did not start on port $Port"
}

function Post-Demo($Base, $Path, $Body) {
    Invoke-RestMethod ("$Base" + $Path) -Method Post -ContentType 'application/json; charset=utf-8' -Body ($Body | ConvertTo-Json -Depth 8)
}

function Wait-Audit($Base, $Run) {
    for ($i = 0; $i -lt 120; $i++) {
        $result = Invoke-RestMethod "$Base/api/audit/$($Run.session_id)"
        if ($result.done) {
            if (!$result.report) { throw "Audit failed: $($result.error)" }
            return $result.report
        }
        Start-Sleep -Milliseconds 100
    }
    throw "Audit did not finish: $($Run.session_id)"
}

function Assert-Audit($Label, $Report, $Decision, $Fen, [string[]]$Hits = @()) {
    $result = $Report.result
    $actualHits = @($result.hits | Where-Object { $_ } | Sort-Object) -join ','
    $expectedHits = @($Hits | Sort-Object) -join ','
    if ($result.decision -ne $Decision -or $result.approved_fen -ne $Fen -or $actualHits -ne $expectedHits) {
        throw "$Label expected $Decision / $Fen / [$expectedHits], got $($result.decision) / $($result.approved_fen) / [$actualHits]"
    }
    Write-Output "PASS $Label | $Decision | $Fen fen | [$actualHits]"
}

$binary = Join-Path $state 'reimburse-demo.exe'
$oldDeepSeek = $env:DEEPSEEK_API_KEY
$oldAnthropic = $env:ANTHROPIC_API_KEY
$ready = $false
Push-Location $repo
try {
    go build -o $binary ./cmd/serve
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed' }

    # These child processes run only on synthetic extraction even if the host has keys.
    $env:DEEPSEEK_API_KEY = $null
    $env:ANTHROPIC_API_KEY = $null
    $default = Start-DemoServer 'default' $DefaultPort
    $municipal = Start-DemoServer 'municipal' $MunicipalPort 'plugins/reimburse-audit/policy/presets/municipal-soe/policy.json'

    if ($Verify) {
        Assert-Audit 'D01 default (30 days)' (Wait-Audit $default (Post-Demo $default '/api/audit' @{case_id='D01'})) 'approve' 55300
        Assert-Audit 'D01 municipal (20 days)' (Wait-Audit $municipal (Post-Demo $municipal '/api/audit' @{case_id='D01'})) 'need_more_info' 0 @('R07-a')

        $first = Post-Demo $municipal '/api/audit' @{case_id='D02'}
        Assert-Audit 'D02 before supplement' (Wait-Audit $municipal $first) 'need_more_info' 0 @('R14-a')
        $taskUrl = "/api/tasks/$($first.task_id)"
        $supplement = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'demo-itinerary.txt') -Raw -Encoding utf8
        $submitted = Post-Demo $municipal "$taskUrl/resubmit" @{kind='support'; name='合成铁路行程单'; content=$supplement}
        if ($submitted.task.state -ne 'pending') { throw 'D02 did not return to pending after supplement' }
        Assert-Audit 'D02 after supplement' (Wait-Audit $municipal (Post-Demo $municipal "$taskUrl/reaudit" @{})) 'approve' 55300

        $timeout = Post-Demo $municipal '/api/audit' @{case_id='D03'}
        Assert-Audit 'D03 verification timeout' (Wait-Audit $municipal $timeout) 'manual_review' 0 @('R15-a')
        $manual = Invoke-RestMethod "$municipal/api/tasks/$($timeout.task_id)"
        if ($manual.task.state -ne 'need_manual') { throw 'D03 did not enter the manual review queue' }

        $clean = Post-Demo $municipal '/api/audit' @{case_id='D04'}
        Assert-Audit 'D04 first submission' (Wait-Audit $municipal $clean) 'approve' 128000
        $review = Post-Demo $municipal '/api/review' @{case_id='D04'; session_id=$clean.session_id; decision='approve'; decision_before='approve'; note='演示确认'}
        if (!$review.impact.affects_dedup) { throw 'D04 review did not update duplicate history' }
        Assert-Audit 'D05 duplicate invoice' (Wait-Audit $municipal (Post-Demo $municipal '/api/audit' @{case_id='D05'})) 'reject' 0 @('R04-a')
    } else {
        Write-Output "Municipal demo: $municipal/?case=D01"
        Write-Output "Default comparison: $default/?case=D01"
        Write-Output "Server process IDs: $($servers.Id -join ', ')"
    }
    Write-Output "Demo state: $state"
    $ready = $true
} finally {
    $env:DEEPSEEK_API_KEY = $oldDeepSeek
    $env:ANTHROPIC_API_KEY = $oldAnthropic
    Pop-Location
    if ($Verify -or !$ready) {
        foreach ($process in $servers) {
            if (!$process.HasExited) { Stop-Process -InputObject $process }
        }
    }
}
