@echo off
setlocal EnableDelayedExpansion
set MAX_WAIT_ATTEMPTS=60

echo Starting multi-kind (ServiceAccount / ConfigMap / LimitRange) test flow...
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
echo ==> Show v1 objects
kubectl -n app-sandbox get serviceaccount,configmap,limitrange
timeout /t 2 /nobreak >nul

echo.
echo ==> Upgrade NamespaceClass to v2 (ConfigMap data + LimitRange)
kubectl apply -f config/samples/namespaceclass-multikind-v2.yaml
timeout /t 2 /nobreak >nul

call :wait_multikind_v2
if errorlevel 1 exit /b 1

echo.
echo ==> Show v2 objects
kubectl -n app-sandbox get serviceaccount,configmap,limitrange
timeout /t 2 /nobreak >nul

echo.
echo ==> ConfigMap data (expect profile=v2 and note key)
kubectl -n app-sandbox get configmap class-config -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> LimitRange detail
kubectl -n app-sandbox get limitrange mem-limit-range -o yaml
timeout /t 2 /nobreak >nul

echo.
echo ==> Delete managed ServiceAccount to simulate drift
kubectl -n app-sandbox delete serviceaccount app-runner --wait=true
timeout /t 2 /nobreak >nul

call :wait_sa_recreated
if errorlevel 1 exit /b 1

echo.
echo ==> Verify ServiceAccount after reconcile
kubectl -n app-sandbox get serviceaccount app-runner -o yaml
timeout /t 2 /nobreak >nul

echo.
echo Multi-kind test flow complete.
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

:wait_multikind_v2
echo.
echo ==> Wait until NamespaceClass v2 is reconciled (profile=v2 + LimitRange)
for /l %%I in (1,1,%MAX_WAIT_ATTEMPTS%) do (
  set "PROF="
  for /f "usebackq delims=" %%A in (`kubectl -n app-sandbox get configmap class-config -o jsonpath^="{.data.profile}" 2^>nul`) do (
    set "PROF=%%A"
  )
  if "!PROF!"=="v2" (
    kubectl -n app-sandbox get limitrange mem-limit-range >nul 2>nul
    if not errorlevel 1 (
      echo Ready: profile=v2 and mem-limit-range exists
      timeout /t 2 /nobreak >nul
      exit /b 0
    )
  )
  echo Waiting (%%I/%MAX_WAIT_ATTEMPTS%^)...
  timeout /t 2 /nobreak >nul
)
echo Timed out waiting for multi-kind v2 reconcile.
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
