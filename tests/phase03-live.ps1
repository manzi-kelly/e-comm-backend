# Standalone Phase 03 smoke test. Only the explicitly isolated test DB below is permitted.
# The caller supplies DATABASE_URL in its process environment; no secrets are printed.
$ErrorActionPreference = 'Stop'
$dsn = [uri]$env:DATABASE_URL
if ($dsn.Host -ne '127.0.0.1' -or $dsn.Port -ne 55433 -or $dsn.AbsolutePath -ne '/autivashop_phase03_test') {
    throw 'This script requires the isolated Phase 03 test database on loopback port 55433.'
}
$pgbin = 'C:\Program Files\PostgreSQL\18\bin'
$dbPassword = [uri]::UnescapeDataString(($dsn.UserInfo -split ':',2)[1])
$env:PGPASSWORD = $dbPassword
$count = & "$pgbin\psql.exe" -X -A -t -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c 'SELECT count(*) FROM admin_users;'
if ($LASTEXITCODE -ne 0 -or [int]$count -ne 0) { throw 'Test database must have no existing administrators.' }
$runRoot = Join-Path 'D:\backend\var' ('phase03-live-'+[guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $runRoot | Out-Null
$results = [System.Collections.Generic.List[object]]::new()
$email = 'phase03-live-'+[guid]::NewGuid().ToString('N')+'@example.test'
$password = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$apiProcess = $null
$productId = $null
try {
    $env:APP_ENV='test'
    $env:API_HOST='127.0.0.1'
    $env:API_PORT='18083'
    $env:PRODUCT_IMAGE_DIR=Join-Path $runRoot 'images'
    $env:CORS_ORIGINS='http://localhost:3000'
    $env:CORS_ALLOW_CREDENTIALS='true'
    $env:AUTH_COOKIE_SECURE='false'
    & 'D:\backend\autivashop-api.exe' migrate
    if ($LASTEXITCODE -ne 0) { throw 'Migration command failed.' }
    $env:AUTIVASHOP_ALLOW_BOOTSTRAP='true'
    $env:BOOTSTRAP_ADMIN_EMAIL=$email
    $env:BOOTSTRAP_ADMIN_DISPLAY_NAME='Isolated Phase 03 HTTP tester'
    $env:BOOTSTRAP_ADMIN_PASSWORD=$password
    $env:BOOTSTRAP_ADMIN_ROLE='super_admin'
    & 'D:\backend\autivashop-api.exe' bootstrap-admin
    if ($LASTEXITCODE -ne 0) { throw 'Bootstrap command failed.' }
    $results.Add(@{check='bootstrap CLI';status='PASS'})
    & 'D:\backend\autivashop-api.exe' bootstrap-admin 2> (Join-Path $runRoot 'bootstrap-refusal.log')
    if ($LASTEXITCODE -eq 0) { throw 'Repeated bootstrap was not refused.' }
    $results.Add(@{check='repeated bootstrap refused';status='PASS'})
    Remove-Item Env:BOOTSTRAP_ADMIN_PASSWORD
    Remove-Item Env:AUTIVASHOP_ALLOW_BOOTSTRAP
    $apiProcess=Start-Process -FilePath 'D:\backend\autivashop-api.exe' -WorkingDirectory 'D:\backend' -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runRoot 'api.log') -RedirectStandardError (Join-Path $runRoot 'api-error.log')
    $base='http://127.0.0.1:18083/api/v1'
    $session=[Microsoft.PowerShell.Commands.WebRequestSession]::new()
    function Request([string]$method,[string]$path,[int]$want,$body=$null,[string]$csrf='', [long]$version=0,$form=$null) {
        $headers=@{Origin='http://localhost:3000'}
        if ($csrf) { $headers['X-CSRF-Token']=$csrf }
        if ($version -gt 0) { $headers['If-Match']='"'+$version+'"' }
        $params=@{Method=$method;Uri=$base+$path;WebSession=$session;Headers=$headers;SkipHttpErrorCheck=$true;TimeoutSec=10}
        if ($null -ne $form) {
            $handler=[System.Net.Http.HttpClientHandler]::new()
            $handler.CookieContainer=$session.Cookies
            $client=[System.Net.Http.HttpClient]::new($handler)
            $message=[System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($method),$base+$path)
            $multipart=[System.Net.Http.MultipartFormDataContent]::new()
            foreach ($file in $form.files) {
                $part=[System.Net.Http.ByteArrayContent]::new([IO.File]::ReadAllBytes($file.FullName))
                $part.Headers.ContentType=[System.Net.Http.Headers.MediaTypeHeaderValue]::new('image/png')
                $multipart.Add($part,'files',$file.Name)
            }
            $message.Content=$multipart
            foreach ($key in $headers.Keys) { $message.Headers.Add($key,$headers[$key]) }
            try {
                $uploadResponse=$client.SendAsync($message).GetAwaiter().GetResult()
                try {
                    if ([int]$uploadResponse.StatusCode -ne $want) { throw "$method $path returned $($uploadResponse.StatusCode), expected $want" }
                    $results.Add(@{check="$method $path";status=[int]$uploadResponse.StatusCode})
                    return ($uploadResponse.Content.ReadAsStringAsync().GetAwaiter().GetResult()|ConvertFrom-Json)
                } finally { if ($null -ne $uploadResponse) { $uploadResponse.Dispose() } }
            } finally { $message.Dispose(); $client.Dispose() }
        } elseif ($null -ne $body) { $params.Body=ConvertTo-Json -InputObject $body -Depth 10 -Compress; $params.ContentType='application/json' }
        $response=Invoke-WebRequest @params
        if ([int]$response.StatusCode -ne $want) { throw "$method $path returned $($response.StatusCode), expected $want" }
        $results.Add(@{check="$method $path";status=[int]$response.StatusCode})
        if ($response.Headers['Content-Type'] -like 'application/json*') { return ($response.Content|ConvertFrom-Json) }
    }
    for ($i=0;$i -lt 100;$i++) {
        try { $probe=Invoke-WebRequest -Uri ($base+'/health') -TimeoutSec 1; if ($probe.StatusCode -eq 200) {break} } catch { Start-Sleep -Milliseconds 100 }
        if ($apiProcess.HasExited) { throw 'API process exited before readiness.' }
    }
    Request GET /health 200 | Out-Null
    Request GET /ready 200 | Out-Null
    $bootstrap=Request GET /admin/auth/me 401
    $csrf=$bootstrap.csrfToken
    Request GET /admin/products 401 | Out-Null
    Request POST /admin/auth/login 403 @{email=$email;password=$password} 'invalid-csrf' | Out-Null
    $login=Request POST /admin/auth/login 200 @{email=$email;password=$password} $csrf
    $csrf=$login.csrfToken
    $cookie=$session.Cookies.GetCookies([uri]$base)|Where-Object Name -eq 'autiva_admin_session'
    if (!$cookie.HttpOnly) { throw 'Session cookie must be HttpOnly.' }
    Request GET /admin/auth/me 200 | Out-Null
    $productInput=@{name=@{en='Isolated live product';rw=$null};brand=$null;model=$null;category='phones-and-tablets';description=@{en='Test description';rw='Ibisobanuro'};priceRwf=120000;comparisonPriceRwf=$null;stockQuantity=3;availability='available';location=@{country='RW';district=$null;sector=$null};lowStockThreshold=2;condition='USED_GRADE_A';specifications=@{ram='8 GB'}}
    $p=Request POST /admin/products 201 $productInput $csrf
    $productId=$p.id
    Request GET "/products/$productId" 404 | Out-Null
    Request PATCH "/admin/products/$productId" 422 @{priceRwf=0} $csrf $p.version | Out-Null
    $p=Request PATCH "/admin/inventory/$productId" 200 @{stockQuantity=5} $csrf $p.version
    Request PATCH "/admin/inventory/$productId" 409 @{stockQuantity=9} $csrf 1 | Out-Null
    # Two simultaneous absolute stock updates share a precondition: exactly one wins.
    $raceHandler=[System.Net.Http.HttpClientHandler]::new()
    $raceHandler.CookieContainer=$session.Cookies
    $raceClient=[System.Net.Http.HttpClient]::new($raceHandler)
    $messages=@();$tasks=@();$responses=@()
    try {
        foreach ($stock in 7,8) {
            $message=[System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Patch,$base+"/admin/inventory/$productId")
            $message.Headers.Add('Origin','http://localhost:3000')
            $message.Headers.Add('X-CSRF-Token',$csrf)
            $message.Headers.Add('If-Match','"'+$p.version+'"')
            $message.Content=[System.Net.Http.StringContent]::new('{"stockQuantity":'+$stock+'}',[Text.Encoding]::UTF8,'application/json')
            $messages+=$message;$tasks+=$raceClient.SendAsync($message)
        }
        foreach ($task in $tasks) { $responses+=$task.GetAwaiter().GetResult() }
        $codes=@($responses|ForEach-Object {[int]$_.StatusCode}|Sort-Object)
        if ($codes.Count -ne 2 -or $codes[0] -ne 200 -or $codes[1] -ne 409) { throw 'Concurrent stock updates did not produce one success and one conflict.' }
        $results.Add(@{check='concurrent inventory updates';status='200 + 409'})
    } finally { foreach($response in $responses){$response.Dispose()};foreach($message in $messages){$message.Dispose()};$raceClient.Dispose() }
    $p=Request GET "/admin/products/$productId" 200
    # Force an audit write failure after product/inventory inserts, then verify rollback.
    & "$pgbin\psql.exe" -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c "ALTER TABLE admin_audit_events ADD CONSTRAINT phase03_live_audit_failure CHECK (action <> 'product.created') NOT VALID;" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not install isolated audit failure constraint.' }
    try {
        $rollbackInput=$productInput.Clone();$rollbackInput.sku='PHASE03-LIVE-ROLLBACK'
        Request POST /admin/products 500 $rollbackInput $csrf | Out-Null
        $remaining=& "$pgbin\psql.exe" -X -A -t -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c "SELECT count(*) FROM products WHERE sku='PHASE03-LIVE-ROLLBACK';"
        if ($LASTEXITCODE -ne 0 -or [int]$remaining -ne 0) { throw 'Failed product audit left a partial product.' }
        $results.Add(@{check='product/inventory/audit transaction rollback';status='PASS'})
    } finally {
        & "$pgbin\psql.exe" -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c 'ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS phase03_live_audit_failure;' | Out-Null
    }
    Add-Type -AssemblyName System.Drawing
    $files=@()
    foreach ($i in 1,2) {
        $path=Join-Path $runRoot "image-$i.png"
        $bitmap=[System.Drawing.Bitmap]::new(8,8)
        $bitmap.SetPixel(0,0,[System.Drawing.Color]::FromArgb(255,($i*50),0,0))
        $bitmap.Save($path,[System.Drawing.Imaging.ImageFormat]::Png)
        $bitmap.Dispose()
        $files+=Get-Item -LiteralPath $path
    }
    $p=Request POST "/admin/products/$productId/images" 201 $null $csrf $p.version @{files=$files}
    if ($p.photos.Count -ne 2) { throw 'Expected two images.' }
    $image=$p.photos[0].id
    Request GET "/products/$productId/images/$image" 404 | Out-Null
    Request GET "/admin/products/$productId/images/$image" 200 | Out-Null
    & "$pgbin\psql.exe" -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c "ALTER TABLE admin_audit_events ADD CONSTRAINT phase03_live_image_audit_failure CHECK (action <> 'product.images.uploaded') NOT VALID;" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not install isolated image audit failure constraint.' }
    try {
        # A distinct decoded image passes deduplication and reaches the audit write.
        $thirdPath=Join-Path $runRoot 'rollback-image.png'
        $bitmap=[System.Drawing.Bitmap]::new(8,8);$bitmap.SetPixel(0,0,[System.Drawing.Color]::Blue);$bitmap.Save($thirdPath,[System.Drawing.Imaging.ImageFormat]::Png);$bitmap.Dispose()
        Request POST "/admin/products/$productId/images" 500 $null $csrf $p.version @{files=@((Get-Item -LiteralPath $thirdPath))} | Out-Null
        $persisted=Request GET "/admin/products/$productId" 200
        $storedFiles=@(Get-ChildItem -LiteralPath $env:PRODUCT_IMAGE_DIR -File)
        if ($persisted.version -ne $p.version -or $persisted.photos.Count -ne 2 -or $storedFiles.Count -ne 2) { throw 'Image audit failure left partial metadata or orphan files.' }
        $results.Add(@{check='image metadata/file rollback';status='PASS'})
    } finally {
        & "$pgbin\psql.exe" -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c 'ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS phase03_live_image_audit_failure;' | Out-Null
    }
    $p=Request PATCH "/admin/products/$productId/images/order" 200 @{items=@($p.photos[1].id,$image);primaryId=$image} $csrf $p.version
    $p=Request POST "/admin/products/$productId/publish" 200 $null $csrf $p.version
    Request GET "/products/$productId" 200 | Out-Null
    Request GET "/products/$productId/images/$image" 200 | Out-Null
    Request GET '/products?search=Isolated&category=phones-and-tablets&sort=price-asc&limit=2' 200 | Out-Null
    $p=Request POST "/admin/products/$productId/unpublish" 200 $null $csrf $p.version
    Request GET "/products/$productId/images/$image" 404 | Out-Null
    $p=Request DELETE "/admin/products/$productId/images/$image" 200 $null $csrf $p.version
    Request DELETE "/admin/products/$productId" 204 $null $csrf $p.version | Out-Null
    Request GET "/products/$productId" 404 | Out-Null
    Request POST /admin/auth/logout 204 $null $csrf | Out-Null
    Request GET /admin/auth/me 401 | Out-Null
    $csrf=(Request GET /admin/auth/me 401).csrfToken
    $csrf=(Request POST /admin/auth/login 200 @{email=$email;password=$password} $csrf).csrfToken
    Request POST /admin/auth/logout-all 200 $null $csrf | Out-Null
    Request GET /admin/auth/me 401 | Out-Null
    $results|ConvertTo-Json -Depth 5|Set-Content (Join-Path $runRoot 'results.json')
    Write-Output "PASS: $($results.Count) standalone CLI/HTTP checks. Results: $runRoot\results.json"
} finally {
    if ($null -ne $apiProcess -and !$apiProcess.HasExited) { Stop-Process -Id $apiProcess.Id; $apiProcess.WaitForExit() }
    # Only server-generated UUIDs and the internally generated test email are used.
    if ($productId -match '^[a-f0-9-]{36}$') {
        & "$pgbin\psql.exe" -X -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c "BEGIN; DELETE FROM product_images WHERE product_id='$productId'; DELETE FROM product_specifications WHERE product_id='$productId'; DELETE FROM inventory WHERE product_id='$productId'; DELETE FROM products WHERE id='$productId'; COMMIT;" | Out-Null
    }
    & "$pgbin\psql.exe" -X -h 127.0.0.1 -p 55433 -U phase03_owner -d autivashop_phase03_test -c "DELETE FROM admin_users WHERE email='$email';" | Out-Null
    Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:BOOTSTRAP_ADMIN_PASSWORD -ErrorAction SilentlyContinue
    Remove-Item Env:AUTIVASHOP_ALLOW_BOOTSTRAP -ErrorAction SilentlyContinue
}
