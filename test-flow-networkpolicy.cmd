@echo off
setlocal EnableDelayedExpansion
set MAX_WAIT_ATTEMPTS=60

echo Starting NetworkPolicy / AI approval test flow...
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply CRD namespaceclasses
kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply CRD namespaceclasschangerequests
kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply sample namespace classes
kubectl apply -f config/samples/namespaceclass-public-internal.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply sample namespace web-portal
kubectl apply -f config/samples/namespace-web-portal.yaml
timeout /t 2 /nobreak >nul

call :wait_public_ready
if errorlevel 1 exit /b 1

echo.
echo ==> Switch web-portal to internal-network
kubectl label namespace web-portal namespaceclass.akuity.io/name=internal-network --overwrite
timeout /t 2 /nobreak >nul

echo.
echo ==> Check networkpolicy list
kubectl -n web-portal get networkpolicy
timeout /t 2 /nobreak >nul

echo.
echo ==> Check allow-public-ingress (expected missing after switch)
kubectl -n web-portal get networkpolicy allow-public-ingress
timeout /t 2 /nobreak >nul

echo.
echo ==> Check allow-vpn-only details
kubectl -n web-portal get networkpolicy allow-vpn-only -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Wait for watcher analysis on internal-network
kubectl get namespaceclass internal-network -o yaml
timeout /t 2 /nobreak >nul

echo.
call :wait_recommendation_id internal-network INTERNAL_REC_ID
if errorlevel 1 exit /b 1
echo Internal recommendation ID: !INTERNAL_REC_ID!

set /p INTERNAL_OK=Please update spec.recommendationID in config/samples/namespaceclass-change-request-internal.yaml then type Y to continue: 
if /I not "%INTERNAL_OK%"=="Y" (
  echo Aborted.
  exit /b 1
)
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply internal change request
kubectl apply -f config/samples/namespaceclass-change-request-internal.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Re-check networkpolicy list
kubectl -n web-portal get networkpolicy
timeout /t 2 /nobreak >nul

echo.
echo ==> Re-check allow-public-ingress
kubectl -n web-portal get networkpolicy allow-public-ingress
timeout /t 2 /nobreak >nul

echo.
echo ==> Re-check allow-vpn-only details
kubectl -n web-portal get networkpolicy allow-vpn-only -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Hold auto-heal briefly so watcher can capture drift
kubectl annotate namespaceclass public-network namespaceclass.akuity.io/ai-drift-hold-seconds=45 --overwrite
timeout /t 2 /nobreak >nul

echo.
echo ==> Delete allow-public-ingress after hold is enabled
kubectl -n web-portal delete networkpolicy allow-public-ingress
timeout /t 2 /nobreak >nul

echo.
echo ==> Wait for watcher analysis on public-network
kubectl get namespaceclass public-network -o yaml
timeout /t 2 /nobreak >nul

echo.
call :wait_recommendation_id public-network PUBLIC_REC_ID
if errorlevel 1 exit /b 1
echo Public recommendation ID: !PUBLIC_REC_ID!

set /p PUBLIC_OK=Please update spec.recommendationID in config/samples/namespaceclass-change-request.yaml then type Y to continue: 
if /I not "%PUBLIC_OK%"=="Y" (
  echo Aborted.
  exit /b 1
)
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply public change request
kubectl apply -f config/samples/namespaceclass-change-request.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Verify public change request status
kubectl get namespaceclasschangerequest public-network-approval -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Verify public-network class status
kubectl get namespaceclass public-network -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Verify web-portal network policies
kubectl -n web-portal get networkpolicy
timeout /t 2 /nobreak >nul

echo.
echo NetworkPolicy test flow complete.
endlocal
exit /b 0

:wait_public_ready
echo.
echo ==> Wait until web-portal is stable on public-network
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  kubectl -n web-portal get networkpolicy allow-public-ingress >nul 2>nul
  if not errorlevel 1 (
    set "LAST_CLASS="
    for /f "usebackq delims=" %%A in (`kubectl get namespace web-portal -o jsonpath^="{.metadata.annotations.namespaceclass\.akuity\.io/last-class}" 2^>nul`) do (
      set "LAST_CLASS=%%A"
    )
    if "!LAST_CLASS!"=="public-network" (
      echo Ready: allow-public-ingress exists and last-class=public-network
      timeout /t 2 /nobreak >nul
      exit /b 0
    )
  )
  echo Waiting (%%I/%MAX_WAIT_ATTEMPTS%^)...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for initial public-network state.
exit /b 1

:wait_recommendation_id
set "CLASS_NAME=%~1"
set "OUT_VAR=%~2"
echo.
echo ==> Wait for recommendation ID on %CLASS_NAME%
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  set "REC_ID="
  for /f "usebackq delims=" %%A in (`kubectl get namespaceclass %CLASS_NAME% -o jsonpath^="{.status.recommendations[0].id}" 2^>nul`) do (
    set "REC_ID=%%A"
  )
  if defined REC_ID (
    set "%OUT_VAR%=!REC_ID!"
    timeout /t 2 /nobreak >nul
    exit /b 0
  )
  echo No recommendation yet (%%I/%MAX_WAIT_ATTEMPTS%^)...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for recommendation on %CLASS_NAME%.
exit /b 1
