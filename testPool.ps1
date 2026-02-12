# Test mempool capacity
$start = Get-Date
for ($i=1; $i -le 5500; $i++) {
    ./simchain-cli.exe -connect 127.0.0.1:50001 -cmd submit -data "tx_spam_$i"
}
$end = Get-Date
Write-Host "Duration: $(($end-$start).TotalSeconds) seconds"
