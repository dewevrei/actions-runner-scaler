# Plan: 배포 job용 self-hosted runner 자동 확장 (from INTENT.md and SPEC.md)

Status: accepted.

## Files that change

- `go.mod`, `go.sum`: scaleset v0.5.0, `moby/moby/client`, `BurntSushi/toml`, `golang-jwt/jwt/v5`
- `cmd/actions-runner-scaler/`: `main.go`(하위 명령 분기, 종료 코드), `run.go`, `status.go`, `deletescaleset.go`, `service.go`, `version.go`, `*_test.go`
- `internal/config/`: `config.go`, `validate.go`, `units.go`(ByteSize, Duration), `*_test.go`
- `internal/logging/`: `logging.go`, `logging_test.go`
- `internal/retry/`: `backoff.go`, `classify.go`, `errors.go`(`OpError`, `RateLimitError`, sentinel), `ratelimit.go`, `*_test.go`
- `internal/github/`: `scaleset.go`, `session.go`(메시지 세션 어댑터), `rest.go`, `errors.go`(오류 변환, 상태 기록 RoundTripper), `*_test.go`
- `internal/docker/`: `spec.go`(네트워크, BuildKit, runner 설정), `client.go`, `errors.go`, `exposure.go`(R79), `*_test.go`, `integration_test.go`
- `internal/gate/`: `policy.go`, `token.go`, `server.go`, `*_test.go`
- `internal/scaler/`: `runner.go`, `table.go`, `scaler.go`, `start.go`, `judge.go`, `busy.go`, `watch.go`, `reconcile.go`, `cleanup.go`, `drain.go`, `clock.go`, `*_test.go`, `fakes_test.go`
- `internal/status/`, `internal/service/`(`launchagent.go`), `internal/daemon/`(`daemon.go`, `startup.go`, `session.go`, `adapters.go`): 각 `*_test.go` 포함
- `image/`: `Dockerfile`, `entrypoint.sh`, `job-started.sh`, `build.sh`, `scripts_test.go`
- `e2e/`: `e2e-success.yml`, `e2e-unlisted.yml`, `e2e-caller.yml`, `e2e-env-override.yml`, `e2e-guard.yml`, `actions/e2e-payload/`(action.yml, payload.sh)
- `config.example.toml`, `README.md`, `PLAN.md`

## Order of work

각 마일스톤은 그 마일스톤까지의 코드만으로 `gofmt -l .`(출력 없음), `go vet ./...`, `go test -race ./...`를 통과해야 한다. M2부터 `shellcheck image/*.sh`도 통과해야 한다. 뒤 마일스톤의 패키지는 가짜로 대신한다. 컨테이너 Env, `GET /v1/verdict`, status socket과 출력, 설정 표의 형식은 SPEC Design "외부 계약"을 따른다. 실제 GitHub, Docker, 클러스터를 쓰는 단계는 실행 전에 사용자 승인을 받고, 만든 자원은 직후 지우고 보고한다. 용어 "끝난 runner"(`done`)는 SPEC Design의 정의를 따른다.

**M0. PLAN.md**
1. 사용자가 승인하면 `Status: accepted`로 바꾼다. (no R: 문서)

**M1. 기반**
1. 의존성을 추가하고 버전을 고정한다. (no R: 전제)
2. `internal/config` (R28, R30, R68)
   - `Load`: TOML 디코드 → `MetaData.Undecoded()`로 모르는 키 수집 → 기본값 → `Validate()`가 오류를 모두 `errors.Join`으로 모은다. 범위는 SPEC 설정 표를 옮긴다.
   - `[[pools]]`는 목록이고 개수가 1이 아니면 "이번 버전은 pool 1개만 지원한다". 기본값: runner 1 CPU·`1g`·512, BuildKit 2 CPU·`2g`·1024, max 2, idle 0, allowed-events `["push"]`. 키 파일 `mode & 0o077 != 0`이면 거부하고, App 개인키 PEM을 파싱해 실패하면 설정 오류로 더한다(값은 로그에 쓰지 않음).
3. `internal/logging` (R31): `RedactingHandler`가 등록한 비밀 값(JIT, 설치 토큰, job 확인 토큰)과 URL 쿼리를 메시지, attr, 오류 문자열에서 `<redacted>`로 바꾼다.
4. `internal/retry` (R24, R25, R41, R50, R52, R95, R97)
   - `retry`는 scaleset, `github`, `docker`를 import하지 않는다. `interface{ HTTPStatus() int }`, `OpError{Op, Err}`, `RateLimitError{RetryAfter, Reset}`, sentinel `ErrRunnerGone`, `ErrJobStillRunning`, `ErrRunnerExists`를 정의하고, `github`와 `docker`가 자기 오류를 이것들로 바꾼다.
   - `Backoff{Initial 1s, Max 60s}`와 지터. `Wait(ctx, err)`는 속도 제한이면 `RetryAfter`·`Reset`까지와 백오프 중 긴 쪽, 값이 없으면 최소 60초(R52).
   - `Classify(ctx, op, err) Class`는 분류만 하고 반응은 호출 위치가 SPEC 재시도 표대로 정한다(R97). 순서:
     1. 부모 ctx 취소 → `Canceled`. 호출 하나의 30초 deadline 초과는 `Transient`
     2. `Gone`: `ErrRunnerGone`, `op`가 `RemoveRunner`인 404, `op`가 Docker 삭제(컨테이너, 네트워크, 볼륨)인 not found
     3. `ErrJobStillRunning` → `Transient`. `ErrRunnerExists` → `Permanent`. scaleset이 둘을 409 `ConflictError`로도 감싸므로 409 처리보다 먼저 본다(확인함: scaleset v0.5.0 `errors_test.go` 161~162, 200~201행).
     4. `RateLimitError` → `RateLimited`. 세션 생성의 409, `op`가 Docker 삭제인 409·403 → `Transient`(R50)
     5. `op`가 `GetMessage`, `DeleteMessage`, `AcquireJobs`, 세션 갱신인 404 → `SessionLost`(R25)
     6. 전송 오류, 5xx → `Transient`. 그 밖의 4xx(403, 422, 세션 생성 404, 세션 갱신 뒤 401 포함) → `Permanent`(R41). 상태를 알 수 없는 나머지 → `Transient`
   - `ratelimit.go`: `NextAllowed`가 호출 종류마다 "다음 허용 시각"을 뮤텍스로 가진다(R52). 판정의 `GetRun`은 보지도 정하지도 않는다(R52의 예외, R65).

     | 종류 | 기록·확인하는 곳 | 허용 시각 전일 때 |
     |---|---|---|
     | JIT | runner 시작 | 시작을 건너뜀(R87) |
     | `AcquireJobs` | `Scale`, 세션 루프 | `Scale`은 부르지 않고 오류를 반환해 ack되지 않게 함. 세션 루프는 세션과 이 시각 중 늦은 쪽까지 기다림 |
     | 세션(생성, `GetMessage`, `DeleteMessage`) | 세션 루프 | 기다린 뒤 새 세션 |
     | `RemoveRunner` | 축소, 시작 실패 정리(R18), 거부 정리, 감시와 대조(R101 포함), 마무리(R21, R55, R92), R17, `delete-scaleset` | 축소는 남은 제거 중단(R95). R18 정리와 감시는 그 시각까지 미룸. 거부 정리는 M4-2대로 계속 다시 시도 |
     | `ListRepoRunners` | R17, R103, `delete-scaleset` | R17은 기다리지 않고 건너뜀. R103은 그 시각까지 부르지 않음 |
     | `GetRunnerByName`, `GetRunnerScaleSet`, get-or-create, `SetSystemInfo` | R17, 시작 순서, `delete-scaleset` | R17은 로그 후 건너뜀(`GetRunnerScaleSet`이면 R17 전체를 건너뜀). 시작 순서는 그 시각까지 기다린 뒤 다시(R24) |

     한 번만 부르는 호출(마무리, R17)이 허용 시각 전이면 부르지 않고 다음 시작의 R17에 맡긴다. `delete-scaleset`은 별도 프로세스라 R52대로 기다린 뒤 다시 부른다.
5. 하위 명령 골격, 종료 코드 매핑(신호 종료 0, 그 밖 1), `config.example.toml`(자리표시자만). (R84, R85)

**M2. 이미지와 Docker 연동**
1. `image/Dockerfile` (R32, R35, R69)
   - `FROM ghcr.io/actions/actions-runner:<버전>@sha256:<index digest>`, runner 버전은 v2.338.2로 고정한다(SPEC의 hook 근거가 이 버전이다). aws CLI v2, kubectl, docker CLI, buildx를 버전과 sha256으로 고정해 설치하고, git이 없으면 더한다. `dockerd`, `containerd`가 있으면 지운다. 도구 버전은 파일 상단에 출처 주석과 모은다.
   - `job-started.sh`를 `/usr/local/bin/scaler-job-started.sh`로 설치한다. runner는 `.sh` hook을 `<bash 절대 경로> -e <경로>`로 돌린다(확인함: actions/runner v2.338.2 `HostContext.cs` 812~835행, `ScriptHandlerHelpers.cs` 19행).
   - `USER runner`(uid 1001), `ENV ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/scaler-job-started.sh`, `ENTRYPOINT ["/usr/local/bin/scaler-entrypoint"]`, `CMD ["./run.sh"]`
2. `entrypoint.sh` (R36, R39, R59, R70, R71)
   - `read -r jit`, `read -r token`(실패나 빈 값이면 `exit 1`) → `umask 077` → `~/.scaler/token`(0400)과 `~/.scaler/endpoint`(0400, 첫 줄 `SCALER_JOBCHECK_URL`, 둘째 줄 `SCALER_RUNNER_NAME`)를 쓴다.
   - `docker buildx create --driver remote --name scaler --use "$BUILDKIT_HOST"`(buildkitd 준비를 기다리지 않음) → `export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"`, `unset jit token` → `exec "$@"`
3. `job-started.sh` (R10, R39, R66, R82)
   - 첫 줄에서 `PATH=/usr/bin:/bin`, 명령은 절대 경로로 부른다. 주소와 이름은 endpoint 파일에서만, 토큰은 토큰 파일에서만 `read` 내장 명령으로 읽고 Env를 읽지 않는다.
   - 조회: `printf 'Authorization: Bearer %s\nX-Runner-Name: %s\n' … | /usr/bin/curl -q --noproxy '*' -sS --connect-timeout 2 --max-time 5 -H @- -o <본문 파일> -w '%{http_code}'`. 인증 헤더를 인자에 넣지 않는다. 본문은 jq 없이 `case` 패턴으로 보고, 본문(`-o`)과 상태 코드(`-w`)는 섞지 않는다. `-w` 출력도 파일로 받아 `read`로 읽는다(명령 치환은 앞에서 기다려 `wait`를 무력화하므로 쓰지 않음).
   - allow를 받을 때만 토큰 파일을 지우고 `exit 0`. deny, 401, pending, 연결 실패, 그 밖의 응답은 2초 뒤 다시 조회하고 스스로 끝나지 않는다. 스케일러가 컨테이너를 지울 때까지 돈다.
   - `bash -e` 아래이므로 조회, 파일 읽기, 대기의 모든 실패를 `|| …`로 잡아 일찍 끝나지 않게 한다.
   - curl과 2초 대기는 백그라운드로 띄우고 내장 `wait`로 기다린다. 그래서 INT, TERM을 받으면 trap이 자식이 끝나기를 기다리지 않고 바로 돈다. 자식 하나의 상한은 주석 달린 상수 `CHILD_MAX=5`(초)다. runner는 step 취소 때 SIGINT 뒤 7.5초, SIGTERM 뒤 2.5초를 기다려 프로세스 트리를 죽이므로 7초 미만으로 둔다(확인함: v2.338.2 `ProcessInvoker.cs` 32~33, 443~465행).
   - `trap`을 EXIT, INT, TERM에 건다. allow로 끝날 때가 아니면 두 집합을 `kill -KILL`(내장 명령)로 끝내고 pid를 stderr에 남긴다. PID 1과 자기 자신은 건너뛴다.
     1. 조상 사슬: `START_PID`(기본 `$PPID`, hook의 부모인 Worker)를 `stat`을 읽기 전에 먼저 kill 대상에 넣고, 그 위로 `PROC_DIR/<pid>/stat`의 넷째 필드(ppid, 마지막 `)` 뒤에서 읽음)를 따라 PID 1 직전까지
     2. 이름: `PROC_DIR/[0-9]*/cmdline`의 argv[0]이 `Runner.Listener`나 `Runner.Worker`로 끝나는 프로세스(Worker의 argv[0] 형식은 미확인, M7-4에서 확인)
   - 조상 사슬은 `stat`을 읽지 못하거나 ppid가 0, 1, 자기 자신이면 멈추고, 최대 16단계까지만 오른다.
   - 1은 언제나 부모를 포함하므로 이름이 하나도 맞지 않아도 아무것도 죽이지 않는 경로가 없다. 고르지 않은 방법: `kill -KILL -1`(PID 1과 호출자를 뺀 전부). 이유: 대상을 가짜로 바꿀 수 없어 호스트에서 도는 `scripts_test`가 개발 PC의 사용자 프로세스를 모두 죽이게 된다.
   - listener가 알 수 없는 코드로 끝나면 `run.sh`가 0으로 끝나 컨테이너가 종료된다(확인함: v2.338.2 `run-helper.sh.template` 78~80행, `run.sh` 13~27행).
   - 상수(`STATE_DIR`, `PROC_DIR`, `START_PID`, `CHILD_MAX`, 간격)는 파일 상단 한 줄씩이고, bash 3.2에서도 돈다. `BASH_ENV`, `LD_PRELOAD` 방어는 runner가 workflow `env:`를 hook에 넘기지 않는다는 사실에만 기댄다(우려 11).
4. `image/build.sh` (R35, R76): BuildKit 이미지 `moby/buildkit:<버전>-rootless@sha256:<index digest>`를 이 파일 상단 한 곳에 고정하고 `docker pull --platform linux/arm64`한다. runner 이미지를 빌드하고 `runner.image`(이미지 ID)와 `buildkit.image` 값을 출력한다. `--record`면 판정 조회 전에 hook 명령줄(`/proc/$PPID/cmdline`, `/proc/self/cmdline`)과 환경 변수 이름 목록(값 제외)을 `~/.scaler/hook-record`에 쓰는 hook 변형을 넣은 별도 태그를 만든다(M7-4 첫 실행용).
5. `internal/docker` (R6, R12, R18, R32, R33, R34, R36, R42, R50, R62, R63, R70, R72, R73, R74, R75, R76)
   - 순수 함수 `NetworkSpec`, `BuildKitSpec`, `RunnerSpec(Params)`. 이름은 `<runner>-net`, `<runner>-buildkit`, `<runner>`, 라벨은 `managed-by`, `runner-name`, `kind`(network, buildkit, runner)
   - runner: `User=runner`, `CapDrop=[ALL]`, `SecurityOpt=[no-new-privileges]`, `Privileged=false`, `Init=true`, 마운트 없음, Env는 R70의 세 개만, `OpenStdin`·`StdinOnce`·`AttachStdin`, linux/arm64
   - BuildKit: Cmd `--addr tcp://0.0.0.0:1234 --oci-worker-no-process-sandbox`, `CapDrop=[ALL]`, `CapAdd=[SETUID, SETGID]`, `SecurityOpt=[seccomp=unconfined]`만, `Privileged=false`, 비root, 마운트·포트 공개 없음
   - 두 컨테이너 모두 `NanoCPUs`, `Memory`, `MemorySwap=Memory`, `PidsLimit`를 pool 값으로 둔다.
   - `Client.StartRunner(ctx, p, stdin)`: 네트워크 → BuildKit 생성·시작 → runner 생성 → attach → 시작 → stdin 두 줄 → CloseWrite. 실패하면 만든 것을 역순으로 지운다.
   - `RemoveResources(ctx, name)`: 두 컨테이너를 `Force`, `RemoveVolumes`로, 그다음 네트워크를 지운다. `RemoveContainer(ctx, name)`: runner 컨테이너만(거부 경로와 R53). 그 밖에 `Wait`, `List`(라벨), `Processes`(top API), `ImageExists`, `MemTotal`, `Ping`. 오류는 `cerrdefs` 4xx를 `HTTPStatus()`로 내놓고 `OpError`로 감싼다.
6. `exposure.go` (R79): `127.0.0.1:2375`에 TCP로 붙어 `GET /_ping`이 200과 `OK`면 노출. 다이얼러를 주입한다.
7. `image/scripts_test.go`(실제 Docker 없음) (R10, R39, R66, R82): 스크립트를 임시 폴더로 복사해 상수 줄만 바꾸고, 임시 `HOME`, `PATH` 맨 앞의 가짜 `docker`(받은 인자만 기록), 가짜 `PROC_DIR`(테스트가 띄운 `sleep` 프로세스들의 pid, ppid 사슬을 담은 `stat`, `Runner.Listener`·`Runner.Worker` argv[0])와 그 `sleep` 하나를 가리키는 `START_PID`로 돌린다. hook은 `bash -e <파일>`로 `httptest` 서버에 돌린다.
8. `integration_test.go`(승인 후) (R12, R32, R33, R34, R35, R36, R62, R63, R69, R70, R71, R72, R73, R74, R75, R76): M3 전에 SPEC 미확인 사실을 확인한다. 두 줄 stdin과 데몬 없는 `buildx create`(우려 7), 사용자 정의 네트워크에서 `host.docker.internal`로 호스트 listener 접속(우려 10), BuildKit index digest에 linux/arm64 항목이 있고 `ImageExists`가 그 참조로 성공하는지, 맞춤 seccomp 프로필(우려 8. 되면 SPEC 변경 절차), top API 시그니처, 이미지의 curl이 `-H @-`를 받는지(7.55 이상 필요, 미확인)

**M3. 스케일러 핵심**
1. `internal/github/scaleset.go`, `session.go`, `errors.go` (R22, R25, R41, R46, R51, R52, R80)
   - Transport에 `HTTP2Config{SendPingTimeout 30s, PingTimeout 10s}`. 재시도는 `DefaultRetryConfig()`에서 `OnRateLimit`만 `(0, false)`로 바꿔 `WithRetry`로 넘긴다(확인함: scaleset v0.5.0 `http_client.go` 256, 384행).
   - get-or-create는 runner group id 1, `Labels` 비움, `RunnerSetting.DisableUpdate=true`. 기존 scale set이 false면 Warn 로그. 세션 어댑터가 `listener.Client` 호출마다 `Op`를 붙인다.
   - 오류 변환: `wrapResponseErrorType`은 400, 401, 404, 409와 속도 제한만 감싸고 나머지는 문자열 오류다(확인함: scaleset v0.5.0 `errors.go` 248~267행).
     1. sentinel은 그 코드로(`BadRequestError` 400, `UnauthorizedError` 401, `NotFoundError` 404, `ConflictError` 409), `RunnerNotFound`·`JobStillRunning`·`RunnerExists`는 `retry` sentinel로, 속도 제한은 `retry.RateLimitError`로
     2. 그 밖은 `WithHTTPClient`로 감싼 RoundTripper가 기록한 상태를 쓴다. 칸은 어댑터가 호출마다 ctx에 넣는 atomic 값이고, RoundTrip마다 먼저 비운 뒤 응답이 있을 때 400 이상만 쓴다. 마지막 시도가 전송 오류면 칸이 비어 전송 오류로 분류된다. 200 응답의 디코드 오류는 `Transient`다.
     3. 고정 버전 오류 문자열의 `status="NNN` 해석은 1, 2로 정하지 못할 때만 쓴다.
2. `internal/github/rest.go` (R9, R17, R52, R65, R103): App JWT(RS256)를 직접 서명하고 설치 토큰을 만료 5분 전까지 캐시한다. `GetRun`(타임아웃 30초, `referenced_workflows`는 없음·null·`[]` 구분), `ListRepoRunners`(모든 페이지, 이름·id·`busy`). 속도 제한 → `retry.RateLimitError`, 그 밖 → `*StatusError{Code}`
3. `internal/gate/token.go`, `policy.go`(순수 코드, 판정 타입 포함) (R9, R14, R38, R40)
   - 토큰: 32바이트 `crypto/rand`, base64url(43자), 메모리에만 두고 `subtle.ConstantTimeCompare`로 비교
   - `Evaluate(Job, Run, Policy) Decision`이 R9의 아홉 조건을 모두 보고 거부 이유를 돌려준다. `parseWorkflowRef`는 마지막 `@`에서 나누고, 해석 실패나 빈 값은 거부, owner/repo는 대소문자 무시, ref와 path는 정확히 일치, `referenced_workflows`는 `[]`일 때만 통과
4. 패키지 의존 방향 (no R: 패키지 구조): `scaler` → `gate`(정책, 토큰, 타입). `gate` 서버(M4)는 자기 패키지의 작은 인터페이스로 표를 받고 `daemon`이 연결한다. `go list -deps`로 순환이 없는지 확인한다.
5. `runner.go`, `table.go`, `clock.go`(주입 시계): runner 표 (R1, R2, R3, R7, R37, R61, R86, R94, R101, R102)
   - 상태: `starting`, `idle`, `busy`, `done`, `exiting`, `removed`. 행은 토큰, 판정(`pending`, `allow`, `deny`), busy 시각, `JobStarted`의 job ID와 필드, 첫 `JobCompleted` 시각(다시 받아도 바뀌지 않음), 판정 시작·제거 포기(R94)·R101 삭제 시작 표시를 갖는다.
   - 전이는 뮤텍스 안에서 현재 상태를 확인한 뒤 바꾼다. `BeginExit`, `BeginJudging(name) bool`은 한 호출자만 이긴다. `allow`는 판정이 `pending`이고 상태가 idle이나 busy일 때만 간다(`BeginExit` 뒤에는 거부되고 조회는 계속 `pending`). `deny`는 `pending`에서만 간다. 뮤텍스를 잡은 채 Docker나 GitHub를 부르지 않는다.
   - `MarkBusy(name, at)`는 첫 호출만 시각을 남기고 주입한 콜백(M4의 타이머)을 부른다. `done`에는 아무것도 하지 않는다. draining, 연속 시작 실패 수, 마지막 시작 오류와 시각, 다음 시작 가능 시각도 표가 가진다.
6. `Scaler.Scale(ctx, msg)` (R1, R2, R3, R4, R5, R7, R37, R51, R52, R53, R87, R89, R91, R92, R93, R94, R95, R102)
   - 순서: `JobStarted` → `JobCompleted` → `AcquireJobs`(drain 중에도) → 개수와 시작·제거. `AcquireJobs`의 일시 오류, `SessionLost`, `RateLimited`는 앞의 처리를 마친 뒤 반환하고(R25, R52), 허용 시각 전이면 부르지 않고 오류를 반환한다. 영구 오류는 `fatal`로 보내고 nil(R51).
   - `JobStarted`: busy 표시, `BeginJudging`이 이길 때만 판정 시작(작은 인터페이스, M3에서는 가짜). `done`인 runner가 다른 job ID의 `JobStarted`를 받으면 거부로 보고 M4-2의 거부 경로로 지우고, 같은 job ID면 `done`으로 둔다(R102).
   - `JobCompleted`: `JobStarted`를 받았고 job ID가 다르면 로그만 남기고 상태를 바꾸지 않는다(R102). 아니면 `done`과 첫 수신 시각. nil 메시지와 nil `Statistics`는 직전 통계, 초기 메시지(-1)도 처리. 모르는 이름은 Debug 로그
   - 개수: `live = starting + idle + busy`, `all = live + done + exiting`. `done`은 R1, R2, R37의 수에서 빼고 R3의 수에는 넣는다. `target = min(max, idle설정 + TotalAssignedJobs)`, draining이면 0. `toStart = max(0, min(target − live, max − all))`(draining이면 0), `toRemove`는 제거 포기 표시가 없는 idle 중 오래된 것부터(R37은 idle만).
   - 제거: `RemoveRunner` 성공·`Gone` → `BeginExit` 후 정리. `Transient` → idle 유지, 다음 `Scale`에서 다시(R93). drain 밖의 `JobStillRunningError`도 idle 유지. drain 중의 `JobStillRunningError` → busy로 표시해 신호 drain은 R92로, 오류 drain은 R55로 기다린다(R91, R53). `Permanent` → 제거 포기 표시, 로그와 마지막 오류(R94). `RateLimited` → 남은 제거 중단(R95).
   - drain 중에는 `Scale`마다 모든 idle을 R37로 지운다. 신호 때 `starting`이던 runner는 idle이 되면 지우고 busy가 되면 기다린다.
   - 시작은 메시지를 받은 그 `Scale` 안에서 바로 한다(R4). 속도 제한 대기, 시작 백오프, 일시 오류 1회 실패는 R87대로 60초 측정에서 뺀다.
7. `start.go` (R2, R18, R61, R63, R64, R96)
   - 이름 `<pool>-<8자 hex>`로 `starting` → JIT → 토큰 → `docker.StartRunner` → idle, 감시 등록. JIT 뒤 실패하면 `BeginExit` 후 자원 삭제와 `RemoveRunner`, JIT 실패면 `RemoveRunner` 건너뜀
   - 단계 사이마다 마무리 표시를 확인해 멈춘다. 마무리의 병렬 정리 뒤에 만들어진 컨테이너는 같은 시작 경로가 반환 전에 지운다.
   - 일시·영구 오류 모두 시작 실패로만 센다(drain 없음, 다음 poll에서 다시). 3번 연속 실패 뒤 백오프 10초부터 두 배, 상한 5분, 첫 성공에서 초기화
8. `watch.go`, `reconcile.go` (R3, R6, R42, R50, R67, R83, R98, R101, R103)
   - runner마다 `ContainerWait`(타임아웃 없음) → `BeginExit`에서 이기면 토큰 폐기 → `RemoveResources` → `RemoveRunner`. 일시 오류는 백오프 최대 10번, 영구 오류는 재시도 없이 로그와 마지막 오류(drain 없음). 재시도는 멈춤 신호를 받는다(R55).
   - 대조는 고정 30초 ticker로 돈다. 회차마다 표만 보고 R101 삭제를 외부 호출보다 먼저 시작하고, R103은 한 번에 하나만 도는 별도 고루틴에 20초 deadline을 걸어 대조를 막지 않는다.
   - 대조는 `runner-name` 라벨로 한다. 표에만 있는 행은 종료처럼(`starting` 제외), runner 컨테이너만 있으면 강제 삭제, 짝 없는 BuildKit과 네트워크 삭제. 영구 오류로 삭제에 실패한 ID는 다시 시도하지 않는다. idle runner의 BuildKit이 없거나 멈춰 있으면 R37 규칙으로 지운다(R67).
   - R83: `done`을 빼고 허용 판정이 없는 runner의 `Processes`에 `Runner.Worker`가 있으면 `MarkBusy`.
   - R103: `done`을 빼고 허용 판정이 없는 runner가 있으면 `ListRepoRunners`의 모든 페이지를 읽어 `busy`가 true인 runner를 `MarkBusy`. 20초 deadline은 모든 페이지를 합친 목록 전체에 걸고, `MarkBusy` 직전에 표 뮤텍스 안에서 "허용 판정 없음, `done` 아님"을 다시 확인한다. 실패하면 로그 후 그 대조에서 건너뛰고(drain 없음, R98), 속도 제한이면 R52의 시각까지 부르지 않는다. 같은 오류(403 등)가 이어지면 처음과 내용이 바뀔 때만 로그와 마지막 오류에 남긴다.
   - R101: 첫 `JobCompleted`부터 5분이 지난 `done` runner는 그 뒤 첫 대조가 runner마다 한 번만 `BeginExit` → 자원 삭제와 `RemoveRunner`를 시작한다(drain 중에도). 오류는 위 감시 규칙을 따른다. 30초 주기라 5분 30초 안에 컨테이너가 없어진다.
9. `cleanup.go` (R16, R17, R24, R46)
   - R16: `managed-by` 컨테이너(RemoveVolumes)와 네트워크를 모두 지운다. 목록의 일시 오류는 백오프로 성공할 때까지(R24), 영구 오류는 로그 후 계속. 대상 하나의 실패는 로그 후 건너뜀
   - R17: `GetRunnerScaleSet`으로 그 pool의 scale set ID를 조회만 하고, 없으면 정리할 등록이 없으므로 건너뜀. 그 조회의 일시 오류는 목록과 같이 1, 2, 4초 뒤 최대 3번, 영구 오류나 속도 제한이면 로그 후 R17을 건너뜀. `ListRepoRunners`의 일시 오류는 1, 2, 4초 뒤 최대 3번, 그래도 실패하거나 영구면 로그 후 건너뜀, 속도 제한이면 기다리지 않고 건너뜀. 접두사가 맞는 runner마다 `GetRunnerByName`(실패하면 그 runner만 건너뜀)의 `RunnerScaleSetID` 일치와 살아 있는 컨테이너 없음을 확인한 뒤 `RemoveRunner`

**M4. job gate**
1. `internal/gate/server.go` (R14, R38, R40, R43, R66)
   - `GET /v1/verdict`만, 다른 경로와 메서드는 404이고 경로를 Info 로그에 남긴다(M7-5의 증거). `ReadHeaderTimeout 5s`, `WriteTimeout 10s`. listen은 `jobcheck.listen`(loopback만)이고 status는 제공하지 않는다.
   - 이름으로 행을 찾고 토큰을 상수 시간으로 비교한다. 실패하면 busy를 기록하지 않고 바로 401. 맞으면 `MarkBusy` 뒤 200과 판정
2. `internal/scaler/judge.go` (R9, R10, R11, R38, R65, R98, R102)
   - `BeginJudging`에서 이긴 runner마다 고루틴 하나: `GetRun`(30초, 재시도 없음) → `gate.Evaluate` → 판정. 조회 실패는 일시·영구 모두 그 job만 거부(drain 없음). 같은 `JobStarted`가 다시 와도 판정은 다시 시작하지 않는다.
   - 허용: `pending`이고 idle이나 busy이면 `allow`로 바꾸고 타이머를 멈춘다.
   - 거부(R102의 경우 포함): `BeginExit` → `RemoveContainer`(동기, 10초) → 삭제가 확인될 때만(not found 포함) `pending`에서 `deny`로 → BuildKit, 네트워크, 토큰, `RemoveRunner` 정리. 이유는 Info 로그
   - 삭제가 실패하거나 10초를 넘으면 판정은 `pending`으로 둔다. hook은 계속 조회하므로 step은 돌지 않는다. `BeginExit`에서 이긴 쪽(판정 고루틴이나 타이머)이 오류 종류와 관계없이 삭제가 확인될 때까지 2초마다 다시 지우고, 실패마다 로그와 마지막 오류를 남긴다. 감시나 대조에 기대지 않는다. 마무리나 오류 대기가 시작되면 이 재시도, 180초 타이머, 진행 중 판정(ctx 취소)을 멈춘다.
3. `internal/scaler/busy.go` (R53, R81, R83, R91, R103)
   - 표의 `MarkBusy` 콜백이 `clock.AfterFunc(180s)`를 건다. busy 출처는 토큰이 맞는 첫 hook 조회, `JobStarted`, drain 중의 `JobStillRunningError`, R83과 R103의 발견이다.
   - 타이머가 울릴 때 판정이 `allow`가 아니면 2의 거부 경로로 지운다. drain 중에도 돈다(R92에서 세션 없이 `JobStarted`를 못 받은 job을 지운다). 시계는 주입한다.
4. 이름·토큰 등록과 폐기는 runner 시작과 감시가 맡고, `daemon`이 서버와 표를 잇는다. (R12, R14, R38, R39)

**M5. status, daemon과 drain**
1. `internal/status` (R26, R27, R31, R43, R45, R56, R58, R86): `Recorder`가 마지막 오류와 시각을 가린 뒤 저장한다. 같은 위치의 같은 오류가 이어지면 처음과 바뀔 때만 로그와 마지막 오류에 남긴다. socket 0600, 형식과 출력은 SPEC 외부 계약을 따른다.
2. 신호와 ctx (R20, R21, R48, R57): `signal.Notify`로 SIGTERM, SIGINT. 작업 ctx는 마무리가 끝날 때만 취소하고, listener에는 따로 `listenCtx`를 준다.
3. 시작 순서 (R16, R17, R19, R22, R24, R28, R29, R30, R46, R57, R60, R68, R78, R79, R80)
   1. 잠금(flock). 잡혀 있으면 로그 후 종료 코드 1
   2. 남은 `status.sock`을 지우고 listen, 0600. 실패하면 로그 후 종료 코드 1. state `starting`
   3. 설정 읽기와 검증(App PEM 파싱 포함). 오류는 마지막 오류에 남기고 계속한다.
   4. Docker `Ping` → 5. R16(설정 결과와 관계없이) → 6. 설정 오류면 오류 대기 → 7. 2375 노출 확인 → 8. 두 이미지 존재 확인 → 9. `MemTotal` 경고(R29) → 10. GitHub 클라이언트 생성, R17(`GetRunnerScaleSet`으로 조회만, 없으면 건너뜀) → 11. scale set get-or-create → 12. `SetSystemInfo` → 13. job 확인 서버 listen → 14. 감시 시작 → 15. 첫 세션 → state `running`
   - 4, 5(목록), 8, 11, 12, 15의 일시 오류는 1~60초 백오프로 성공할 때까지(R24, R52). 6, 7과 4, 8, 11, 12, 13, 15의 영구 오류는 오류 대기(R46). 5의 영구 오류는 로그 후 계속, 9의 실패는 경고만
   - 15까지 신호를 받으면 바로 종료 0(R57). 백오프 대기는 신호와 함께 `select`한다.
4. 오류 대기 (R45, R46, R47, R48): 잠금과 status만 유지하고 GitHub와 Docker를 부르지 않는다. 신호를 받으면 종료 0.
5. 세션 루프 (R25, R41, R51, R88, R90, R99)
   - `listener.New(MaxRunners = pool.max)`, drain 중에도 바꾸지 않는다. `listenCtx` 취소면 나간다.
   - 그 밖의 종료: 세션을 닫고(오류는 로그만) `Classify`. `Transient`, `SessionLost`, `RateLimited` → 백오프와, 세션과 `AcquireJobs`의 다음 허용 시각 중 늦은 쪽까지 기다린 뒤 새 세션. `Permanent` → 오류 drain. 신호 drain 중에는 새 세션을 만들지 않는다.
   - `fatal`(세션, scale set, `AcquireJobs`의 영구 오류만)은 `Run`을 멈추지 않고 오류 drain에 들어간다.
6. `internal/scaler/drain.go`의 신호 drain (R20, R21, R89, R90, R91, R92, R100)
   - 첫 세션 뒤 정상 동작 중의 첫 신호(세션 재생성 백오프 중 포함): draining, 새 runner 없음, `AcquireJobs` 계속, idle은 M3-6대로 제거
   - 세션과 job 확인 서버를 유지한다. busy(`done` 포함)와 starting이 0이 되거나, `drain-timeout`이나 두 번째 신호에서 마무리하고 종료 0(오류 대기 없음). 그동안의 영구 오류는 로그만
7. `drain.go`의 오류 drain (R21, R51, R53, R54, R55)
   - draining. `Run`이 살아 있으면 세션과 job 확인 서버를 유지하고, 이미 반환됐으면 세션을 다시 만들지 않는다.
   - idle은 R37로 제거. `JobStillRunningError`면 busy로 대기, 그 밖의 실패면 `RemoveContainer`로 runner 컨테이너만 지운다.
   - busy(`done` 포함)가 모두 끝나거나 `drain-timeout` → 마무리(M5-8, 멈춤 포함) → 오류 대기(GitHub 호출 0회, R47). 그 사이 신호 → 바로 마무리, 종료 0
8. 마무리 (R21, R55, R92, R99): 마무리 표시를 켜고 `WithoutCancel` + 45초 예산 하나로(각 호출의 deadline은 평소 값과 남은 예산 중 작은 쪽) 감시, 대조, R103 고루틴, 거부 삭제 재시도, 180초 타이머, 진행 중 판정 중지 → `listenCtx` 취소, `Run` 반환 최대 10초 → `removed`가 아닌 모든 행(`exiting` 포함, `BeginExit`를 누가 이겼는지와 관계없이)마다 병렬로 자원 삭제와 `RemoveRunner` 1회(최대 20초) → 진행 중인 시작 경로가 스스로 정리를 마칠 때까지 기다림(45초 예산 안) → 세션 닫기(10초, 오류는 로그만) → 서버 중지. 이 순서는 종료와 오류 대기 진입에 같이 쓴다. 남은 것은 다음 시작의 R16, R17이 정리한다.
9. status 상태 (R26, R45, R56, R58, R86): daemon이 state를 바꾼다. pool 줄의 `busy`는 busy와 `done`, `idle`은 starting과 idle이고, `exiting`과 `removed`는 세지 않는다. 연속 시작 실패도 표에서 읽는다.

**M6. 운영 명령**
1. `delete-scaleset <pool>` (R23, R49): 잠금을 잡지 못하면 아무것도 지우지 않고 1. R17 방식으로 소속 runner를 제거하고 `DeleteRunnerScaleSet` 뒤 `GetRunnerScaleSet`이 `(nil, nil)`인지 확인한다.
2. `internal/service` (R19, R44): plist는 `RunAtLoad` true, `KeepAlive`의 `SuccessfulExit` false, `ThrottleInterval` 60, `ExitTimeOut` = drain-timeout + 60, 로그는 `~/Library/Logs/actions-runner-scaler/`. `install`은 로드돼 있으면 `bootout` 완료를 확인한 뒤 파일을 쓰고 `bootstrap`, `uninstall`은 `bootout` 확인 뒤 파일을 지운다.
3. README: 설치, 이미지 빌드, 설정, `status`, `kickstart -k`, 멈추기, `delete-scaleset`, 2375 노출을 끄는 법, runner 버전 교체(63일 기한). (no R: 문서)

**M7. 실제 시험** (단계마다 승인 후 실행. 시험 자원은 직후 지우고 보고)
1. 전제(사용자): `<test-org>/<test-repo>`에 App을 설치하고 Role, 클러스터, Namespace, 레지스트리 값을 저장소 변수로 등록한다. 시험 workflow, 브랜치, `main` 태그, reusable 호출, 자기 호출 변형을 시험 저장소에 쓰는 것은 각각 승인 후다. (no R: 시험 환경)
   - 시험 pool: allowed-workflows `[".github/workflows/e2e-success.yml"]`, allowed-events `["workflow_dispatch"]`. `e2e-success.yml`의 `on:`은 `workflow_dispatch`(input `push` 기본 false, `minutes` 기본 0), 자기 경로로 거른 `push`(main), `workflow_call`이고 `permissions: id-token: write`다.
   - `e2e-unlisted.yml`, `e2e-caller.yml`, `e2e-env-override.yml`, `e2e-guard.yml`도 main에 둔다. UI의 dispatch는 기본 브랜치에 파일이 있어야 하고, 한 번 실행된 뒤에는 API로 어느 브랜치나 태그에도 dispatch할 수 있다(확인함: GitHub 문서 "Events that trigger workflows"의 `workflow_dispatch` 절).
2. 격리와 이미지 확인(integration) (R12, R32, R33, R34, R35, R36, R69, R70, R72, R73, R74, R75, R76)
   - runner와 BuildKit `docker inspect`: Env가 R70의 세 개뿐, AWS·kube 변수와 마운트 없음, JIT와 토큰이 Env·Cmd·Args·Labels에 없음(시험 코드가 값을 메모리에만 들고 비교), privileged false, CapDrop·CapAdd·SecurityOpt, NanoCpus·Memory·MemorySwap·PidsLimit, 포트 공개 없음
   - BuildKit 볼륨이 익명 하나이고 삭제 뒤 남지 않음. 다른 runner 네트워크에서 이 BuildKit 1234 연결 실패. 사용자 정의 네트워크에서 job 확인 서버 접속(우려 10)
   - runner 이미지 `docker run --rm`: 다섯 도구 버전이 고정값, `uname -m`이 aarch64, `command -v dockerd`가 빔
3. E-scale (R1, R2, R3, R4, R5, R6, R7, R22, R26, R37, R80, R86, R103)
   - 직접 실행 → scale set 생성과 DisableUpdate true, idle 0과 1의 컨테이너 수, `status` 출력. 동시 dispatch 3건에 max 2: `docker ps` 1초 표본에서 runner가 2를 넘지 않음
   - 첫 메시지 시각(로그)부터 job `started_at`까지 60초 이하. job 하나 뒤 컨테이너 종료, 자원과 등록 제거. 실행 중에 BuildKit 이미지를 지워(digest로 다시 pull) 시작 실패를 만들고 `status`의 연속 시작 실패 표시, 다시 pull한 뒤 회복
   - App 토큰의 저장소 runner 목록이 200인지와 job 중 그 runner의 `busy`가 true인지 기록한다(우려 14, R103).
4. E-success, 시험 저장소 main (R8, R39, R59, R71, R77, R83, R101)
   - step: `GITHUB_WORKFLOW_REF`·`GITHUB_RUN_ID` 출력 → `test -z "$ACTIONS_RUNNER_INPUT_JITCONFIG"` → 토큰 파일 없음, `env` 값에 43자 base64url 없음 → `docker buildx ls`의 기본 builder가 원격 `scaler` → OIDC로 Role → Pod 목록 → input `push`가 true일 때만 작은 이미지 `docker buildx build --push`(builder 지정 없음)
   - push는 최대 3번, 몇 MB 이미지, 태그 `<test-name>-<sha>`, 다시 할 때는 새 커밋. 끝나면 삭제를 한 번 시도해 결과를 기록하고(실패가 예상) 남은 태그를 보고한다.
   - 첫 실행은 `--record` 이미지와 workflow `env:`의 `E2E_ENV_MARK=1`, `minutes` 3으로 한다. job이 도는 동안 `docker cp`로 `hook-record`를 꺼내 hook 명령줄(`bash -e <경로>` 여부)과 이름 목록의 `E2E_ENV_MARK` 유무를 기록한다. 있으면 workflow `env:`의 `LD_PRELOAD`, `BASH_ENV`도 hook에 들어간다고 판단한다(우려 11).
   - 기록: `JobWorkflowRef` 형식(첫 실행), 5번 이상 실행한 `JobStarted` 지연 최댓값(우려 4), runner 1GB로 끝나는지(OOMKilled false), 데몬 없는 login 결과(우려 7), job 중 top API에 `Runner.Worker`가 보이는지, `JobCompleted` 수신부터 컨테이너 종료까지의 시간(우려 15, R101의 5분과 비교)
5. E-deny (R9, R10, R11, R65, R66, R82)
   - (a) main이 아닌 브랜치 dispatch, (b) main의 `e2e-unlisted.yml`, (c) workflow를 main에 쓰는 승인된 커밋 자체가 만든 `push` run(스케일러가 도는 중에 하거나, 아니면 큐의 run을 취소), (d) main이 아닌 커밋의 `main` 태그 dispatch, (e) 다른 브랜치의 `e2e-caller.yml`이 main의 reusable을 `@main`으로 호출, (g) main이 아닌 커밋의 `main` 태그에서 허용 workflow가 같은 파일을 `@main`으로 호출
   - (f) `e2e-env-override.yml`이 다른 브랜치에서 `env:`로 `SCALER_JOBCHECK_URL`, `SCALER_RUNNER_NAME`, `PATH`, 프록시 변수, `CURL_HOME`을 덮어쓰고, `BASH_ENV`는 "Set up job"이 내려받는 시험 저장소 action(`e2e-payload`)의 `payload.sh`를 가리킨다. payload가 돌면 job 확인 서버의 고정 경로 `/e2e-payload-ran`을 불러 스케일러 로그에 줄을 남긴다. `LD_PRELOAD`는 M7-4의 hook 환경 기록으로 판단한다.
   - `main` 태그는 (d), (g) 직전에 만들고 직후에 지운다.
   - 통과: Role 요청 step과 `if: always()` 표시 step이 한 번도 돌지 않음, 판정 뒤 10초 안에 runner 컨테이너 없음(로그와 `docker events`), job이 성공으로 끝나지 않음, (f)에서 스케일러 로그에 `/e2e-payload-ran` 0건과 실제 주소로 온 조회(우려 11)
   - 기록: 실패 표시까지의 실제 시간(우려 6), 같은 거부 job에 새 runner가 배정된 횟수, (g)의 `referenced_workflows`(우려 5)
6. E-guard, hook이 기다리는 동안의 취소와 시간 제한 (R10, R66)
   - 소스상 job 취소는 실행 중인 hook을 끊지 않으므로(SPEC Design hook 항목) 이 시험은 trap 경로가 아니라 결과를 확인한다. trap 경로는 `scripts_test`가 증명한다.
   - `e2e-guard.yml`: job 둘(input으로 하나만 실행), 하나는 시간 제한 없음, 하나는 `timeout-minutes: 1`. 각 job의 `if: always()`와 `if: cancelled()` 표시 step은 시험 스크립트가 호스트 loopback의 다른 포트에 띄운 임시 listener를 `host.docker.internal`로 부른다(스케일러는 쓰지 않음).
   - STOP 전 양성 대조: 같은 조건(사용자 정의 네트워크의 컨테이너)에서 임시 listener의 대조 전용 경로 `/control`로 보낸 요청 하나가 닿아야 한다. 닿지 않으면 그 회차는 무효다.
   - 시험 스크립트가 스케일러 로그를 따라 읽다가 그 runner의 "runner 시작" 줄을 보면 바로 `kill -STOP`한다. (h) `gh run cancel`로 취소, (i) 1분 시간 제한을 기다린다. 컨테이너 종료를 본 뒤 `kill -CONT`한다. STOP 전에 그 runner의 판정 로그가 있으면 그 회차는 무효로 보고 다시 한다.
   - 통과: STOP 뒤 표시 step 경로의 요청 0건, REST job 조회(`GET /repos/{owner}/{repo}/actions/jobs/{job_id}`)의 step 결과에서 표시 step이 실행되지 않음, runner 컨테이너가 끝남. 취소부터 컨테이너 종료까지의 시간을 기록한다(우려 6). 임시 listener는 직후 끈다.
7. E-lifecycle, `service install` 뒤 (R16, R17, R19, R20, R21, R44, R84, R88, R89, R90, R91, R92)
   - job 중 `kill -9` → 재시작, 이전 자원 정리, 다음 job 시작까지 5분 이하. 재부팅(사용자) 뒤 job 시작까지 5분 이하
   - job 중 `bootout` → job이 끝난 뒤 종료 0, 다시 띄우지 않음. 두 번째 신호 → 바로 종료. drain 중 큐에 넣은 job이 재시작 뒤 시작되는지(우려 3)
8. E-error (R28, R41, R45, R46, R47, R48, R49, R68, R78, R79, R85)
   - App이 없는 저장소 URL, 모르는 설정 키, pool 2개 → 오류 대기, `status` 1, 5분 동안 GitHub 호출 없음, 신호에 종료 0. 이때 `delete-scaleset`이 1. 두 번째 인스턴스 → 종료 코드 1
   - R79(선택): Docker 설정은 두고, `127.0.0.1:2375`에 `/_ping`에 200 `OK`를 주는 임시 listener를 띄운 뒤 실제 바이너리가 오류 대기에 드는지 본다. listener는 직후 끈다.
9. 정리: `delete-scaleset`, `service uninstall`, 시험 브랜치 삭제(승인 후), 남은 이미지 태그 보고. `<test-repo>` 삭제는 사용자가 한다. (R23)

## Risks

- 가장 위험한 단계: M3-3과 M4-2의 판정. 틀리면 main이 아닌 코드가 OIDC와 push 권한이 있는 runner에서 돈다(AWS 쪽 main 제한은 관리자 정책이 따로 맡는다). `Evaluate`는 순수 함수로 두고 조건마다 하나씩 틀린 표 테스트를 쓰며, 해석 실패는 모두 거부한다.
- 가장 위험한 단계: M3-5~M3-8, M4-2, M4-3, M5의 시간과 상태 전이. `JobStarted`(재전달과 다른 job ID 포함), `JobCompleted`, hook 조회, R83과 R103의 발견, 컨테이너 종료, 180초 타이머, R101의 5분, 축소, drain이 같은 runner에 동시에 닿는다. 틀리면 R3을 어기거나 허용된 job을 지운다. 전이는 표 메서드에 모으고 `BeginExit`, `BeginJudging` 단일 승자, `pending`에서만 가는 판정, 가짜 시계 경쟁 시험을 `-race`로 돌린다.
- 가장 위험한 단계: M2-3의 hook. hook이 허용 없이 끝나면 `always()`와 `failure()` step이 돈다(SPEC Design hook 항목, 우려 6). `bash -e` 아래의 잡지 못한 실패 하나가 그 경로를 연다. 모든 실패를 잡고, 끝날 때는 trap이 runner 프로세스를 먼저 죽인다. hook 자체가 SIGKILL을 받으면 trap이 돌지 않는다.
- 가장 위험한 단계: M2-5의 BuildKit 격리. `seccomp=unconfined`(우려 8)와 process sandbox 없음 때문에 설정 하나가 빠지면 격리가 깨진다. spec 표 테스트와 M7-2 inspect로 두 번 확인한다. 라벨 없는 익명 볼륨이 남으면 찾지 못한다(우려 13).
- `host.docker.internal` 접속(우려 10), 두 줄 stdin, 데몬 없는 `buildx create`(우려 7)가 안 되면 모든 job이 실패한다. M2-8에서 M3 전에 확인하고, 안 되면 SPEC 변경 절차를 밟는다.
- 거부 삭제가 계속 실패하면 step은 돌지 않지만 job은 GitHub의 job 시간 제한까지 hook에 머물고 `max` 자리 하나를 차지한다. 이긴 쪽이 2초마다 다시 지운다.
- 취소와 `timeout-minutes`가 hook을 끊는 경로는 소스로만 봤다(`timeout-minutes`는 미확인). M7-6에서 확인한다.
- R101의 5분 안에 로그 올리기가 끝나지 않으면 마지막 로그를 잃는다(우려 15). M7-4에서 잰다. 시작 전에 취소된 job의 `JobCompleted`가 대기 runner로 오면 그 runner는 R101로 지워진다(우려 16).
- R83과 R103이 모두 실패하면 hook이 닿지 않고 `JobStarted`도 없는 runner가 job 시간 제한까지 남는다(우려 17, 14). `Runner.Worker`가 top API에 그 이름으로 보이는지는 M7-4에서 기록한다.
- 마무리는 `Run` 반환 10초, 병렬 정리 20초, 세션 닫기 10초이고 각 호출 deadline이 남은 예산을 넘지 않으므로 45초 안에 끝나 `ExitTimeOut`(drain-timeout + 60)에 15초 여유가 있다. 마무리 중 시작 중이던 runner는 시작 경로가 스스로 지운다.
- `JobWorkflowRef` 형식이 다르면 모든 job이 거부된다(M7-4 첫 실행). `JobStarted`가 180초 넘게 늦으면 허용될 job도 지워진다(우려 4).
- idle runner를 지우는 순간 job이 배정되는 경쟁이 있다(미확인). 서버가 `JobStillRunning`, `RunnerExists`를 400과 409 중 무엇으로 보내는지 미확인이라 `Classify` 순서로 둘 다 같은 결과를 내게 한다.
- 영구 4xx는 사람이 고쳐야 복구된다(우려 1). 여러 pool은 오류 범위가 정해지기 전에는 받지 않는다(우려 2). sleep/wake 뒤 HTTP/2 ping의 효과는 미확인이다(우려 9). 다른 loopback 서비스 노출은 우려 12다.

## Proof

단위 테스트(`go test -race ./...`)
- `config_test` (R28, R30, R68): 모르는 키 2개와 범위 밖 값 1개 → 오류 3개. 깨진 PEM → 설정 오류. 범위마다 경계 안팎 값. 키 파일 0644 거부, 0600·0400 통과. pool 0개·2개 거부. 기본값이 SPEC 표와 같음
- `logging_test` (R31): 비밀 값과 URL 쿼리가 메시지, attr, 오류에 나타나지 않음
- `backoff_test`, `ratelimit_test` (R24, R52, R95): 1, 2, 4초 … 60초 상한. 종류별 다음 허용 시각이 서로 섞이지 않음
- `classify_test` (R25, R41, R50, R52, R97)
  - `ErrJobStillRunning`, `ErrRunnerExists`를 400과 409로 감싼 경우. 세션 생성 409 → Transient, 그 밖의 409 → Permanent
  - 세션 호출 404 → SessionLost, 세션 생성 404와 세션 갱신 뒤 401 → Permanent. scaleset 403(속도 제한 아님)과 422 → Permanent
  - `RemoveRunner` 404, Docker 삭제 not found → Gone. Docker 삭제 409·403 → Transient, 다른 `op`면 Permanent
  - 부모 ctx 취소 → Canceled, 호출 ctx만 deadline 초과 → Transient. `Wait`: `RetryAfter` 90초면 90초, 값 없으면 60초
- `errors_test`(github) (R41, R46, R51): sentinel별 코드, RoundTripper가 기록한 403·422, 기록이 없을 때만 문자열 해석. 403 뒤 마지막 시도가 전송 오류 → 칸이 비어 Transient. 200 응답의 디코드 오류 → Transient
- `scaleset_test` (R22, R52, R80): `GenerateJitRunnerConfig`가 `Retry-After` 없는 429를 받으면 라이브러리가 다시 보내지 않고 `RateLimitError`를 돌려줌. 생성 요청의 DisableUpdate true, 기존 false면 Warn
- `rest_test` (R9, R17, R52, R65, R103): 속도 제한 → `RateLimitError`. `referenced_workflows` 없음·null·`[]` 구분. runner 목록의 모든 페이지와 `busy` 값
- `spec_test` (R12, R32, R33, R34, R35, R36, R62, R63, R70, R72, R73, R74, R75, R76): 세 설정의 보안, 제한, Env 세 개, 마운트·포트 없음, 라벨, 네트워크 이름
- `client_test`(가짜 Docker API) (R18, R50, R62, R63): 생성 순서, BuildKit이나 runner 생성 실패 때 역순 삭제, `RemoveVolumes` 사용, `RemoveContainer`는 runner 컨테이너만
- `exposure_test` (R79, 단위 테스트 + 선택 시험): `OK` 응답 → 노출, 연결 거부와 다른 응답 → 통과
- `scripts_test` (R10, R39, R66, R82)
  - entrypoint: 두 줄 stdin → 토큰·endpoint 파일 0400, `env`에 토큰 없음, 가짜 `docker`가 받은 인자가 `buildx create --driver remote --name scaler --use <BUILDKIT_HOST>`, 빈 줄이면 1
  - hook(`bash -e`): allow → 0과 토큰 파일 삭제, 가짜 runner 프로세스는 살아 있음. deny, 401, 연결 실패가 이어져도 여러 주기 뒤에 끝나지 않음, 그 뒤 allow → 0
  - hook에 TERM → 가짜 `Runner.Listener`·`Runner.Worker` 프로세스와 조상 사슬이 죽고 hook이 끝남. stderr의 kill 대상에 `1`과 hook 자신이 없음
  - 이름이 맞는 프로세스가 없어도 `START_PID`(부모)와 그 위 사슬이 죽음. `START_PID`의 `stat`이 없어도 `START_PID`는 죽음. 응답이 10초 걸리는 서버를 조회하는 중 INT → kill까지 1초 이하
  - Env의 주소·이름·`PATH`·프록시·`CURL_HOME`을 바꿔도 같은 서버에 같은 헤더로 조회. 프로세스 인자에 토큰 없음
- `policy_test`, `token_test` (R9, R14): 아홉 조건 중 하나씩만 틀린 9경우, 모두 맞음, 값 없음, 해석할 수 없는 ref, `refs/tags/main`, 대소문자만 다른 owner(허용), `referenced_workflows` null과 1개. 기본 `["push"]` 허용 경로는 이 단위 테스트로만 증명한다. 토큰 43자, 다른 토큰과 불일치
- `table_test` (R3, R6, R7, R18, R94, R101, R102): 동시 `BeginExit`에도 정리 1번. `BeginJudging`은 한 번만 참. `allow` 뒤 `deny` 거부, `BeginExit` 뒤 `allow` 거부와 조회 `pending`. `MarkBusy`는 첫 시각만, `done`에는 무시. 첫 `JobCompleted` 시각은 재전달로 바뀌지 않음. 제거 포기 행은 다시 고르지 않음. `removed`는 개수에 들지 않음
- `scaler_test` (R1, R2, R3, R4, R5, R7, R37, R51, R53, R61, R86, R87, R89, R91, R92, R93, R94, R95, R96, R102)
  - 개수 표(max 경계, `done`은 R3에만). 메시지를 받은 `Scale` 안에서 시작 호출. `AcquireJobs` 일시 오류에도 같은 메시지의 `JobStarted`·`JobCompleted`는 처리됨. `AcquireJobs` 허용 시각 전 → 호출 0회와 오류 반환
  - drain: `target` 0, `AcquireJobs` 호출, 시작 0회, 모든 idle 제거 시도, `starting`이 idle이 되면 제거, `JobStillRunningError` → busy. drain 밖 `JobStillRunningError` → idle 유지, busy 아님
  - R102: job ID가 다른 `JobCompleted` → 로그만, 상태 그대로. `done`이 다른 job ID의 `JobStarted`를 받음 → 거부 경로, 같은 job ID → `done` 유지
  - `RemoveRunner` Transient → 다음 `Scale`에서 다시, Permanent → 다시 안 함과 마지막 오류, RateLimited → 남은 제거 0회
  - `AcquireJobs` 영구 → `fatal` 1건과 nil. JIT·컨테이너 영구 오류 → `fatal` 0건, 실패 수 증가, 다음 poll에서 다시. R61: 3번 연속 실패 뒤 10, 20, 40초 … 5분, 첫 성공에서 초기화, status 값에 반영
- `start_test` (R18, R52, R63, R64): JIT 뒤 각 단계 실패 → 자원 삭제와 `RemoveRunner` 1회. JIT 실패 → `RemoveRunner` 0회. JIT `RateLimited` 뒤 다음 JIT 호출은 60초 뒤(가짜 시계). 마무리 표시가 켜지면 다음 단계 0회, 만든 컨테이너 삭제
- `watch_test`, `reconcile_test` (R3, R6, R42, R50, R67, R83, R98, R101, R103)
  - 종료 뒤 정리 1번, 토큰 폐기. 삭제 not found는 성공, 영구 오류 → 재시도 없음과 `fatal` 0건. 대조(가짜 시계)는 라벨로, `starting` 건너뜀, 짝 없는 BuildKit·네트워크 삭제, BuildKit 멈춘 idle → `RemoveRunner` 뒤 삭제
  - R83: `Runner.Worker` 있으면 busy 표시만, `done`은 건너뜀. R103: 목록의 `busy` true·판정 없음·`done` 아님 → busy, 목록 실패 → 건너뜀, 속도 제한 → 그 시각 전 목록 호출 0회
  - R101: 첫 `JobCompleted` 뒤 4분 59초에는 삭제 없음, 5분 뒤 첫 대조에서 삭제 시작 1번, 재전달 뒤에도 기준 시각 그대로, drain 중에도 적용. `ListRepoRunners`가 30초 걸려도 삭제 시작은 5분 30초 안
  - R103의 같은 403이 반복돼도 로그와 마지막 오류는 1번, 내용이 바뀌면 다시
- `cleanup_test` (R16, R17, R46): 이전 자원 모두 삭제, R16 목록 영구 오류 → 로그 후 계속. scale set이 없으면 R17 건너뜀. `GetRunnerScaleSet` 일시 오류는 1, 2, 4초 뒤 3번 후 건너뜀, 영구 오류와 속도 제한은 바로 건너뜀. scale set ID가 다르거나 컨테이너가 살아 있는 runner는 남김. 목록 일시 오류는 1, 2, 4초 뒤 3번 후 건너뜀, 영구와 속도 제한은 바로 건너뜀, `GetRunnerByName` 실패는 그 runner만 건너뜀
- `server_test` (R14, R38, R40, R43, R66): 틀린 토큰과 다른 runner 이름 → 401, busy 기록 없음. 폐기 뒤 401. 같은 토큰의 반복 조회는 같은 판정. `/v1/verdict` 밖의 경로는 404와 경로 로그
- `judge_test` (R9, R10, R11, R38, R65)
  - `GetRun` 일시·영구 오류 → 재시도 없이 거부. 거부 시 runner 컨테이너 삭제가 `deny` 기록보다 먼저, 10초 deadline
  - 삭제 실패 → 조회가 `pending`. 영구 오류여도 2초마다 삭제를 계속 시도. `allow` 뒤 같은 `JobStarted` 재전달, 그 뒤 `GetRun` 실패 → 삭제 0회
- `busy_test` (R53, R81, R83, R91, R103): busy 출처 다섯 가지 중 첫 것에서 정확히 180초에 거부(179.999초에는 아님), 그 전 `allow`면 지우지 않음, drain 중에도 타이머 동작. R81, R83, R103의 시간 경로는 이 가짜 시계 테스트로만 증명한다.
- `daemon_test` (R16, R17, R19, R20, R21, R22, R24, R25, R28, R29, R46, R47, R48, R51, R53, R54, R55, R57, R60, R78, R79, R84, R85, R88, R89, R90, R91, R92, R99, R100)
  - 시작 순서가 잠금 → socket → 설정 → Docker → R16 → 오류 대기 판단 → 2375 → 이미지 → R17 → scale set. 설정 오류여도 R16은 돌고 그 뒤 오류 대기, 마지막 오류에 설정 오류 전부. 잠금이 잡혀 있으면 1, 남은 socket이 있어도 시작, listen 실패면 1
  - 2375 노출, 이미지 없음, job 확인 서버 listen 실패 → 오류 대기, 그 뒤 가짜 호출 0회(감시, 대조, 거부 삭제 재시도, 180초 타이머, 진행 중 판정 포함), 신호에 0. 시작 단계 409·5xx → 백오프 뒤 성공, 속도 제한 → R52 시각까지 대기. 백오프 중 신호 → 0. 메모리 합계 초과 → 경고 후 계속
  - `Run` 오류와 세션 404 → 새 세션, 세션 생성 404 → 오류 drain. `AcquireJobs` 속도 제한 → 새 세션은 세션과 `AcquireJobs` 허용 시각 중 늦은 쪽 뒤. 세션 닫기 실패 → 로그만. 세션 재생성 백오프 중 신호 → 신호 drain, 새 세션 없음
  - 오류 drain: `Run`이 살아 있으면 세션과 서버 유지, idle의 `JobStillRunningError` → busy 대기, 그 밖의 실패 → runner 컨테이너만 삭제, `done` 컨테이너도 기다림, 오류 대기 전에 감시·대조·거부 삭제 재시도, 타이머, 판정을 멈춰 그 뒤 호출 0회. 목록을 읽던 R103 고루틴이 멈추고 그 뒤 `ListRepoRunners` 0회. 오류 drain 중 느린 시작 경로 → 스스로 정리를 마칠 때까지 기다린 뒤 오류 대기, 그 뒤 Docker 호출 0회. 신호 → 0
  - 신호 drain: `MaxRunners` 불변, `AcquireJobs` 계속, 새 runner 0, busy·`done`·starting이 0이 될 때까지 세션 유지 뒤 0. 세션 없이 `JobStarted`를 못 받은 runner는 180초 뒤 삭제. 두 번째 신호 → 바로 0. 영구 오류 → 로그만, 오류 대기 없이 0
  - 마무리: 거부 삭제가 계속 실패 중인 runner도 마무리에서 한 번 지워짐. 목록을 읽던 R103 고루틴이 멈추고 그 뒤 `ListRepoRunners` 0회. 느린 가짜로도 45초 안에 끝나고 runner별 정리가 병렬, 호출 deadline이 남은 예산 이하, 허용 시각 전 `RemoveRunner` 0회. 로그 버퍼에 가짜 JIT와 토큰 값 0건(R31)
- `status_test` (R26, R27, R31, R43, R45, R56, R58, R86): 상태 4가지와 연결 실패의 출력과 종료 코드. `busy`에 `done` 포함, `busy + idle`이 표의 starting·idle·busy·`done` 합과 같고 `exiting`·`removed`는 빠짐. 연속 시작 실패 표시와 0, socket 0600, 오류 문자열 가림, 같은 오류 반복은 1번만 기록
- `deletescaleset_test` (R23, R49): 잠금이 잡혀 있으면 1과 삭제 호출 0회. 잡으면 소속 runner 제거와 삭제 호출
- `launchagent_test` (R19, R44): `SuccessfulExit` false, `ThrottleInterval` 60, `ExitTimeOut` = drain-timeout + 60, uninstall이 파일을 지움

실제 시험(승인 후)
- integration(M2-8): R12, R32, R33, R34, R35, R36, R62, R63, R69, R70, R71, R72, R73, R74, R75, R76
- 격리(M7-2): R12, R32, R33, R34, R35, R36, R69, R70, R72, R73, R74, R75, R76
- E-scale: R1, R2, R3, R4, R5, R6, R7, R22, R26, R37, R80, R86, R103(목록 응답 기록만) / E-success: R8, R39, R59, R71, R77, R83(top API 기록만), R101(시간 기록만)
- E-deny: R9, R10, R11, R65, R66, R82 / E-guard: R10, R66 / E-lifecycle: R16, R17, R19, R20, R21, R44, R84, R88, R89, R90, R91, R92
- E-error: R28, R41, R45, R46, R47, R48, R49, R68, R78, R79(선택), R85 / 정리: R23
- R10: 거부(E-deny), 취소와 시간 제한(E-guard) 모두에서 `always()`·`cancelled()` step 0회와 성공이 아닌 결과. hook이 끝나지 않는 성질은 `scripts_test`가 증명한다.
- R31: 모든 E 시험 로그에서 개인키 머리말과 설치 토큰 접두사 `ghs_`를 grep해 0건. 값은 출력하지 않는다.
- R4, R87: E-scale의 60초 측정에서 R87의 경우에 해당한 job은 로그로 구분해 뺀다.

## Decisions

- 코드 규칙은 AGENTS.md를 따른다.
- 빌드는 runner마다 BuildKit 컨테이너 하나를 전용 네트워크와 함께 둔다. 고르지 않은 방법: 공유 BuildKit(캐시와 결과가 job끼리 섞임), dind(privileged), 호스트 Docker 소켓(호스트 Docker 전체 제어).
- BuildKit 보안 옵션은 `seccomp=unconfined`와 `--oci-worker-no-process-sandbox`다. 고르지 않은 방법: `systempaths=unconfined`(Docker VM 커널의 `/proc`, `/sys` 가림을 품). 맞춤 seccomp 프로필은 M2-8에서 시험하고 되면 SPEC을 고쳐 바꾼다.
- BuildKit 이미지 digest는 `image/build.sh` 상단 한 곳에 index digest로 고정하고 `config.example.toml`에는 자리표시자만 둔다. 고르지 않은 방법: 플랫폼 manifest digest(다시 pull할 때 참조가 달라질 수 있음, 미확인), 설정에만 적기. 2026-11-24 시험의 digest가 index인지는 미확인이라 M2-8에서 확인한다.
- runner 버전은 v2.338.2로 고정한다. 이유: SPEC의 hook 호출, 환경, 취소 경로 근거가 이 버전의 소스다. 바꾸면 M7-4의 기록과 우려 11을 다시 확인한다.
- 이번 버전은 pool 1개만 받되 설정은 처음부터 `[[pools]]` 목록이다. 고르지 않은 방법: 단일 표 설정(pool을 더할 때 설정 파일이 깨짐).
- 판정 상태(토큰, 판정, busy 시각, 판정 시작, `JobCompleted` 시각)는 scaler의 runner 표 한 곳에 둔다. `gate`는 정책, 토큰, HTTP만 갖는다. 고르지 않은 방법: gate가 상태를 따로 가짐(두 상태를 맞춰야 함).
- 거부는 runner 컨테이너 삭제가 확인된 뒤에만 기록하고, hook은 거부를 받아도 끝나지 않는다. 고르지 않은 방법: hook이 거부에서 1로 끝남(`always()` step이 돈다).
- hook은 2초 간격으로 다시 조회한다. 고르지 않은 방법: 판정까지 요청을 붙잡는 long poll(서버가 요청별 상태와 긴 WriteTimeout을 가져야 함).
- hook은 entrypoint가 쓴 파일과 절대 경로만 쓰고 `curl -q --noproxy '*'`, 헤더는 stdin(`-H @-`)으로 넘긴다. runner 프로세스는 외부 도구 없이 `/proc`와 내장 `kill`로 찾고 끝낸다. 고르지 않은 방법: `pkill`(이미지에 있는지 미확인).
- 속도 제한은 호출 종류마다 따로 기다린다(R52의 "같은 호출"). 판정의 `GetRun`은 예외다. 고르지 않은 방법: 하나의 공유 시각(한 종류의 제한이 다른 호출까지 멈춤).
- 오류는 `Classify`가 분류만 하고 반응은 호출 위치가 정한다. 고르지 않은 방법: "영구 오류면 drain" 같은 공통 처리(R97, R98을 어김).
- R17은 scale set을 만들기 전에 `GetRunnerScaleSet`으로 ID를 조회만 한다. 이유: SPEC 시작 순서가 R17을 scale set 단계 앞에 두고, R17은 ID가 필요하다.
- CLI는 표준 `flag`의 하위 명령별 `FlagSet`. 고르지 않은 방법: cobra(명령이 5개뿐).
- Docker는 `moby/moby/client`. 고르지 않은 방법: `docker/docker` 모듈(새 릴리스가 `+incompatible`로 멈췄다고 봄, 미확인), docker CLI(인자 노출, 출력 파싱).
- TOML은 `BurntSushi/toml`. 이유: `MetaData.Undecoded`로 모르는 키를 모두 모은다(R28).
- GitHub REST는 설치 토큰, run 조회, runner 목록 세 엔드포인트만 직접 부른다. 고르지 않은 방법: go-github(의존성이 큼).
- 컨테이너 종료는 runner마다 `ContainerWait`와 30초 대조로 감지한다. 고르지 않은 방법: Docker 이벤트 스트림(끊김과 재구독 처리가 따로 필요하고 대조는 어차피 필요).
- `maxCapacity`는 언제나 pool의 `max`. 고르지 않은 방법: drain 중 낮추기(0의 서버 쪽 효과 미확인).
- 토큰은 메모리에 원문으로 두고 상수 시간으로 비교한다. 고르지 않은 방법: 해시만 저장(토큰은 프로세스 밖으로 나가지 않고 재시작하면 사라짐).
- 시험 pool의 allowed-events는 `["workflow_dispatch"]`다. 고르지 않은 방법: `push` 함께 넣기(E-deny (c)에 `schedule`이 필요해 실행 시각을 정할 수 없음).
- runner 이미지: gosu, sudo, docker 그룹, 컨테이너 안 `config.sh` 등록을 쓰지 않는다(R32, JIT 사용). kustomize는 넣지 않는다(R69에 없음). entrypoint에서 재귀 chown하지 않는다(R4).
- Docker 소켓은 기본 경로 `/var/run/docker.sock`(Docker Desktop에서 되는지는 미확인, M2-8). 시험 저장소는 `<test-org>/<test-repo>`(비공개)다.
