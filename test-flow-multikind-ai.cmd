@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"
set MAX_WAIT_ATTEMPTS=60

REM Match main.go godotenv.Load(): load repo-root .env into this cmd session for the preflight check.
if exist .env (
  for /f "usebackq eol=# tokens=1,* delims==" %%A in (".env") do (
    if not "%%~A"=="" set "%%~A=%%B"
  )
)

if not defined OPENAI_API_KEY (
  echo OPENAI_API_KEY is not set after loading .env ^(if any^). Use repo-root .env, or set the variable, then run the manager with AI enabled, e.g.:
  echo   set OPENAI_API_KEY=your-key
  echo   go run . --ai-poll-interval=2s
  exit /b 1
)

echo Starting multi-kind AI watcher + pull-request gate test flow...
echo (Requires controller built from this repo, with OPENAI_API_KEY set when you run the manager.)
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
echo ==> Apply NamespaceClass multi-kind-demo v1 (SA + ConfigMap)
kubectl apply -f config/samples/namespaceclass-multikind-v1.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply namespace app-sandbox
kubectl apply -f config/samples/namespace-app-sandbox.yaml
timeout /t 2 /nobreak >nul

call :wait_multikind_v1
if errorlevel 1 exit /b 1

echo.
echo ==> Require pull request URL on approvals for multi-kind-demo (annotation)
kubectl annotate namespaceclass multi-kind-demo namespaceclass.akuity.io/require-pull-request=true --overwrite
timeout /t 2 /nobreak >nul

echo.
echo ==> Show class before drift
kubectl get namespaceclass multi-kind-demo -o yaml
timeout /t 2 /nobreak >nul

REM Deleting a managed ServiceAccount is healed immediately by the namespace controller, so the AI
REM watcher often never sees drift. Switch class twice to record switch annotations (see README §7).
echo.
echo ==> Apply empty staging NamespaceClass (for label switch only)
kubectl apply -f config/samples/namespaceclass-multikind-staging.yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Switch app-sandbox to multi-kind-staging (strips demo resources)
kubectl label namespace app-sandbox namespaceclass.akuity.io/name=multi-kind-staging --overwrite
timeout /t 2 /nobreak >nul
timeout /t 2 /nobreak >nul

echo.
echo ==> Switch app-sandbox back to multi-kind-demo (recreates SA/CM; leaves switch drift ~10m)
kubectl label namespace app-sandbox namespaceclass.akuity.io/name=multi-kind-demo --overwrite
timeout /t 2 /nobreak >nul

call :wait_multikind_v1
if errorlevel 1 exit /b 1

echo.
echo ==> Hint: watcher status (expect DriftDetected / recommendation soon)
kubectl get namespaceclass multi-kind-demo -o yaml
timeout /t 2 /nobreak >nul

call :wait_recommendation_id multi-kind-demo MULTIKIND_REC_ID
if errorlevel 1 exit /b 1
echo Recommendation ID: !MULTIKIND_REC_ID!

set /p MULTIKIND_OK=Update spec.recommendationID in config/samples/namespaceclass-change-request-multikind.yaml (pullRequestURL is already set) then type Y to continue: 
if /I not "!MULTIKIND_OK!"=="Y" (
  echo Aborted.
  exit /b 1
)
timeout /t 2 /nobreak >nul

echo.
echo ==> Apply multi-kind change request
kubectl apply -f config/samples/namespaceclass-change-request-multikind.yaml
timeout /t 2 /nobreak >nul

call :wait_cr_applied_or_fail
if errorlevel 1 exit /b 1

call :wait_sa_recreated
if errorlevel 1 exit /b 1

echo.
echo ==> Verify change request (phase, appliedPullRequestURL)
kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Verify namespaceclass status after apply
kubectl get namespaceclass multi-kind-demo -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Verify app-sandbox ServiceAccount
kubectl -n app-sandbox get serviceaccount app-runner -o yaml
timeout /t 2 /nobreak >nul

echo.
echo Multi-kind AI + PR gate test flow complete.
endlocal
exit /b 0

:wait_multikind_v1
echo.
echo ==> Wait until app-sandbox has ServiceAccount + ConfigMap (profile=v1)
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  kubectl -n app-sandbox get serviceaccount app-runner >nul 2>nul
  if not errorlevel 1 (
    kubectl -n app-sandbox get configmap class-config >nul 2>nul
    if not errorlevel 1 (
      set "PROF="
      for /f "usebackq delims=" %%A in (`kubectl -n app-sandbox get configmap class-config -o jsonpath^="{.data.profile}" 2^>nul`) do (
        set "PROF=%%A"
      )
      if "!PROF!"=="v1" (
        echo Ready: app-runner + class-config profile=v1
        timeout /t 2 /nobreak >nul
        exit /b 0
      )
    )
  )
  echo Waiting (%%I/%MAX_WAIT_ATTEMPTS%^)...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for multi-kind v1 reconcile.
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

:wait_cr_applied_or_fail
echo.
echo ==> Wait until NamespaceClassChangeRequest multikind-demo-approval is Applied or Rejected
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  set "PHASE="
  set "MSG="
  for /f "usebackq delims=" %%A in (`kubectl get namespaceclasschangerequest multikind-demo-approval -o jsonpath^="{.status.phase}" 2^>nul`) do (
    set "PHASE=%%A"
  )
  for /f "usebackq delims=" %%A in (`kubectl get namespaceclasschangerequest multikind-demo-approval -o jsonpath^="{.status.message}" 2^>nul`) do (
    set "MSG=%%A"
  )
  if "!PHASE!"=="Applied" (
    echo Change request phase=Applied
    timeout /t 2 /nobreak >nul
    exit /b 0
  )
  if "!PHASE!"=="Rejected" (
    echo Change request rejected: !MSG!
    kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml
    exit /b 1
  )
  echo Waiting (%%I/%MAX_WAIT_ATTEMPTS%^) phase=!PHASE!...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for change request to finish.
kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml
exit /b 1

:wait_sa_recreated
echo.
echo ==> Wait until controller recreates ServiceAccount app-runner
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  kubectl -n app-sandbox get serviceaccount app-runner >nul 2>nul
  if not errorlevel 1 (
    echo ServiceAccount app-runner is present
    timeout /t 2 /nobreak >nul
    exit /b 0
  )
  echo Waiting (%%I/%MAX_WAIT_ATTEMPTS%^)...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for ServiceAccount recreate.
exit /b 1
