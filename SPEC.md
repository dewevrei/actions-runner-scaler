# Spec: 배포 job용 self-hosted runner 자동 확장

From: INTENT.md (dewevrei, fab48f4). Status: accepted.
Skills applied: actions-scaleset

## Requirements

스케일링
- R1. 스케일러는 목표 runner 수를 `min(runners.max, runners.idle + 배정된 job 수)`로 계산한다. 배정된 job 수는 GitHub가 보내는 `statistics.TotalAssignedJobs`다.
- R2. 존재하는 runner 수가 목표보다 적으면 스케일러는 그 차이만큼 runner 컨테이너를 시작한다.
- R3. 존재하는 runner 컨테이너 수는 `runners.max`를 넘지 않는다.
- R4. 존재하는 runner가 `runners.max`보다 적을 때, GitHub job 기록의 `created_at`부터 `started_at`까지 60초를 넘지 않는다.
- R5. runner 컨테이너는 job 하나를 끝내면 종료된다.
- R6. runner 컨테이너가 종료되면 스케일러는 그 컨테이너를 지우고 그 runner의 GitHub 등록을 제거한다.
- R7. 배정된 job이 없으면 runner 컨테이너 수는 `runners.idle`과 같다.
- R37. 존재하는 runner가 목표보다 많으면, 스케일러는 `RemoveRunner`가 성공한 runner의 컨테이너만 지운다. `RemoveRunner`가 실패하면 그 runner는 남긴다.

자격
- R8. 자격을 받은 job은 runner 안에서 `credentials.namespace`의 Pod 목록 조회에 성공한다.
- R9. 스케일러는 다음 다섯 조건을 모두 만족하는 job에만 클러스터 자격을 발급한다.
  - 그 runner의 `JobStarted` 메시지의 `JobWorkflowRef`가 `<대상 저장소>/<허용 workflow 파일>@refs/heads/main`이다.
  - 같은 메시지의 `WorkflowRunID`로 조회한 workflow run의 `head_repository.full_name`이 대상 저장소다.
  - 그 run의 `head_branch`가 `main`이다.
  - 그 run의 `event`가 `credentials.allowed-events`에 있다.
  - 그 run의 `path`가 `credentials.allowed-workflows`에 있다.
- R10. 자격 발급이 거부된 job의 결과는 실패이고, `if` 조건이 없는 step은 실행되지 않는다. `if: always()`나 `failure()` step은 자격 없이 실행될 수 있다.
- R11. 자격 발급이 거부된 job의 컨테이너에는 클러스터 자격이 없다.
- R12. runner 컨테이너에는 job이 시작되기 전까지 클러스터 자격이 없다(대기 runner 포함).
- R13. `credentials.session-duration`을 15분으로 두었을 때, 20분 동안 도는 허용 job은 끝날 때까지 클러스터 읽기에 성공한다.
- R14. 자격 요청 토큰은 발급받은 runner 이름과 그 컨테이너의 수명에 묶인다. 다른 runner 이름으로 쓰거나 컨테이너가 끝난 뒤 쓰면 거부된다.
- R38. 자격 발급이 거부되면 broker는 그 runner의 토큰을 바로 폐기한다.
- R15. runner 컨테이너의 환경 변수, 파일, 라벨에는 호스트의 장기 AWS 키가 없고, 발급하는 자격의 세션 시간은 1시간 이하다.
- R39. 자격 요청 토큰은 job step의 환경 변수로 상속되지 않는다.
- R40. broker는 토큰 검증에 실패한 요청에 바로 401을 돌려주고, `JobStarted` 대기에 들어가지 않는다.

수명주기
- R16. 스케일러는 시작할 때 `managed-by` 라벨이 붙은 이전 runner 컨테이너를 지운다. 이전 실행에서 진행 중이던 job은 이때 실패한다.
- R17. 스케일러는 시작할 때 저장소 runner 목록에서 다음 두 조건을 만족하는 등록을 제거한다.
  - 이름이 `<scaleset.name>-`로 시작하고 `GetRunnerByName`의 `RunnerScaleSetID`가 우리 scale set ID와 같다.
  - 살아 있는 컨테이너가 없다.
- R18. JIT 발급 뒤 컨테이너 생성이나 시작이 실패하면 스케일러는 그 runner 등록을 바로 제거한다.
- R19. 스케일러가 비정상 종료되거나 호스트가 재시작되면, 사람 개입 없이 5분 안에 스케일러가 다시 job을 받는다.
- R20. SIGTERM을 받으면 스케일러는 drain한다.
  - 새 runner를 만들지 않는다.
  - `maxCapacity`를 job을 실행 중인 runner 수로 낮춘다.
  - 대기 runner는 R37의 규칙으로 지운다.
  - 실행 중인 job이 끝날 때까지(최대 `shutdown.drain-timeout`) 세션과 broker를 유지한다.
  - 그 뒤 남은 컨테이너를 지우고 종료한다.
- R21. drain 중에 두 번째 SIGTERM이나 SIGINT를 받으면 기다리지 않고 남은 컨테이너를 지운 뒤 종료한다.
- R22. scale set이 없으면 만들고, 있으면 다시 쓴다. 스케일러가 종료돼도 scale set은 지우지 않는다.
- R23. `delete-scaleset` 명령은 scale set과 그 runner 등록을 지운다.
- R24. 시작 단계의 호출(Docker 준비 확인, scale set get-or-create, 세션 생성)이 실패하면, 1초에서 시작해 최대 60초 간격의 지수 백오프로 성공할 때까지 다시 시도한다.
- R41. 영구 오류(HTTP 400, 401, 403, 404, `RunnerExistsError`)는 같은 호출을 반복하지 않고 로그에 남긴다.
- R25. `listener.Run`이 취소가 아닌 오류로 끝나면 스케일러는 새 세션을 만들어 메시지 수신을 다시 시작한다.
- R42. Docker 이벤트 수신이 끊기면 스케일러는 다시 구독하고, 30초마다 실제 컨테이너 목록과 내부 runner 상태를 맞춘다.

운영과 설정
- R26. 스케일러가 실행 중이면 `status` 명령은 실행 중 runner 수, 대기 runner 수, 마지막 오류와 그 시각을 출력하고 0으로 끝난다.
- R27. 스케일러가 실행 중이 아니면 `status` 명령은 "실행 중 아님"을 출력하고 1로 끝난다.
- R43. 상태 조회는 소유자만 읽을 수 있는 unix socket으로만 제공하고, 컨테이너에서 닿는 broker 주소로는 제공하지 않는다.
- R28. 설정 파일에 모르는 키, 형식 오류, 허용 범위 밖의 값이 있으면 시작을 거부하고, 발견한 오류를 모두 한 번에 출력한다. 허용 범위는 설정 표에 있다.
- R29. `runners.max × runners.memory`가 Docker가 쓸 수 있는 메모리를 넘으면 시작 로그에 경고를 남기고 계속 실행한다.
- R30. GitHub App 개인키 파일의 권한이 0600보다 넓으면 시작을 거부한다.
- R31. 로그, 오류 메시지, `status` 출력에 JIT 설정, GitHub 토큰, 개인키, AWS 자격, 자격 요청 토큰의 값이 나타나지 않는다.
- R44. `service install` 명령은 설정의 `shutdown.drain-timeout`으로 계산한 `ExitTimeOut`을 넣은 LaunchAgent를 설치하고, `service uninstall`은 그것을 지운다.

runner 컨테이너
- R32. runner 컨테이너는 비root 사용자로 돌고, privileged가 아니며, 모든 capability를 제거하고, no-new-privileges가 켜져 있다.
- R33. runner 컨테이너에는 호스트 Docker 소켓을 포함한 호스트 경로가 마운트되지 않는다.
- R34. runner 컨테이너의 CPU, 메모리, 프로세스 수 제한은 설정값과 같고, 메모리 스왑은 없다.
- R35. runner 컨테이너는 linux/arm64 이미지로 만들어진다.
- R36. JIT 설정과 자격 요청 토큰은 컨테이너의 환경 변수, 명령 인자, 라벨(`docker inspect`로 보이는 값)에 나타나지 않는다.

## Design

**구성 요소**
- `cmd/actions-runner-scaler`: 하위 명령 `run`(서비스), `status`, `delete-scaleset`, `service install|uninstall`, `version`.
- GitHub 연동:
  - `actions/scaleset` v0.5.0의 `NewClientWithGitHubApp`
  - workflow run 조회는 GitHub REST(`GET /repos/{owner}/{repo}/actions/runs/{run_id}`)를 같은 App의 설치 토큰으로 호출한다.
  - 둘 다 우리가 쓰는 메서드만 가진 좁은 인터페이스 뒤에 둔다.
  - App 권한 Administration(읽기 쓰기), Actions(읽기), Metadata(읽기)로 scale set 생성, 세션, JIT 발급, runner 제거, scale set 삭제, workflow run 조회가 모두 된다(확인함: 2026-11-20 App 시험). run 응답에는 `head_branch`, `event`, `path`, `head_repository`가 있다(확인함: 같은 날 조회).
- Docker 연동: Docker Engine API Go 클라이언트를 인터페이스 뒤에 둔다.
- broker(자격 중개): 스케일러 안에서 `broker.listen`(기본 `127.0.0.1:8765`)의 HTTP 서버로 돈다.
  - 컨테이너는 `host.docker.internal`로 접속한다(확인함: 2026-11-20, 컨테이너에서 호스트 loopback 서비스로 HTTP 200).
  - 이 주소는 같은 Docker의 다른 컨테이너도 접근할 수 있으므로, 모든 요청은 토큰 검증부터 한다(R40).
  - `status`는 별도 unix socket(`~/.local/state/actions-runner-scaler/status.sock`, 0600)으로 제공한다(R43).
- runner 이미지: `image/`의 Dockerfile로 호스트에서 빌드한다.
  - 기반은 공식 `actions-runner`(linux/arm64, digest 고정)이고, 여기에 kubectl, aws CLI v2, kustomize, jq(버전 고정)와 아래 세 스크립트를 더한다.
  - 빌드한 이미지는 로컬 이미지 ID(`sha256:…`)로 설정에 고정한다. 로컬 빌드 이미지에는 레지스트리 digest가 없기 때문이다. 이미지 ID로 컨테이너를 만들 수 있다(확인함: 2026-11-20). 실행 중에는 pull하지 않는다.
  - 스크립트 세 개:
    - entrypoint 래퍼: stdin의 첫 줄(JIT)을 `ACTIONS_RUNNER_INPUT_JITCONFIG`로 넘기고, 둘째 줄(자격 요청 토큰)은 `~/.scaler/token`(0400)에 쓴 뒤 `run.sh`를 exec한다. 토큰은 환경 변수로 넘기지 않는다(R39). 공식 이미지(arm64, 사용자 `runner`)에서 stdin으로 받은 값은 `docker inspect`의 Env, Cmd, Args에 나타나지 않았고, `run.sh`는 그 환경 변수의 JIT 값을 읽어 해석했다(확인함: 2026-11-20 시험).
    - job 시작 hook(`ACTIONS_RUNNER_HOOK_JOB_STARTED`): 토큰 파일로 broker에 자격을 요청한다. 성공하면 `~/.kube/config`와 `~/.aws/config`를 쓰고, 실패하면 1로 끝난다. hook은 `always()` 조건의 pre-job step으로 실행되고, 0이 아닌 종료 코드는 job을 실패시킨다(확인함: actions/runner `src/Runner.Worker/JobExtension.cs`, `docs/adrs/1751-runner-job-hooks.md`).
    - `credential_process` 스크립트: 토큰 파일로 broker에 자격을 다시 요청한다. AWS 키 환경 변수 없이 `credential_process`만으로 받은 역할 세션 자격으로 `eks get-token`과 Pod 목록 조회(HTTP 200)가 된다(확인함: 2026-11-20 시험).

**스케일링 흐름** (actions-scaleset 스킬의 계약을 따른다)
1. 시작:
   - 설정 검증(R28~R30)과 단일 실행 잠금
   - 정리(R16, R17). JIT로 등록만 되고 컨테이너가 없는 runner는 저장소 runner 목록에 `offline`으로 나타난다. 이 목록의 라벨과 runner group으로는 scale set 소속을 알 수 없지만, `GetRunnerByName`의 `RunnerScaleSetID`로는 알 수 있다. `RemoveRunner`로 지워지고, 이미 지운 runner를 다시 지워도 오류가 없다(확인함: 2026-11-20 시험).
   - 기본 runner group(id 1)에서 scale set get-or-create(R22, R24). `Labels`를 비우면 서버가 이름을 유일한 라벨(`system:<이름>`)로 붙인다(확인함: 2026-11-20 시험, 기본 group `Default` id 1).
2. `SetSystemInfo`를 부른 뒤 세션을 만든다.
   - `ConflictError`(409)면 백오프 재시도(R24).
   - 이전 실행이 닫지 못한 세션은 1분 안에 만료되고, 정상적으로 닫은 세션 뒤에는 바로 만들어진다(확인함: 2026-11-20 시험, 버려진 세션 뒤 30초에 409, 61초에 성공).
   - `listener.Config.MaxRunners`는 `runners.max`(drain 때만 R20대로 낮춤).
3. `Scale()`:
   - nil 메시지와 초기 메시지(-1) 처리
   - `JobAvailable`마다 `AcquireJobs`
   - `JobStarted`의 runner 이름, `JobWorkflowRef`, `WorkflowRunID`를 기록하고 그 runner를 busy로 표시. broker가 이 정보를 쓴다.
   - `JobCompleted` 처리
   - `TotalAssignedJobs`로 목표 수 계산
   - 부족하면 시작(R2, R3), 남으면 R37로 줄임
   - runner 시작 실패는 다시 시도하지 않고 로그만 남긴 뒤 nil을 반환한다. 다음 poll에서 목표 수로 다시 맞춘다. 모르는 runner 메시지도 로그만 남긴다.
4. runner 시작:
   - `GenerateJitRunnerConfig`로 이름 `<scaleset.name>-<8자 임의값>`의 JIT를 발급한다.
   - 컨테이너를 만든다(R32~R35). 라벨에는 `managed-by`, runner 이름, runner ID만 둔다.
   - attach한 stdin으로 JIT와 토큰을 쓰고 닫는다(R36).
   - 생성이나 시작이 실패하면 `RemoveRunner`를 부른다(R18).
5. 감시(R6, R42): Docker 이벤트로 컨테이너 종료를 받으면 컨테이너를 지우고, 토큰을 폐기하고, `RemoveRunner`를 부른다.
   - 제거 실패는 백오프로 최대 10번 다시 시도하고, 그래도 남으면 다음 시작 때 R17이 정리한다.
   - 이벤트 수신이 끊기면 다시 구독하고, 30초마다 컨테이너 목록과 상태를 맞춘다.
6. `listener.Run`이 취소가 아닌 오류로 끝나면 새 세션으로 다시 시작한다(R25).
7. 종료는 R20, R21을 따른다.
   - 세션은 drain이 끝난 뒤 `context.WithoutCancel`에 타임아웃을 붙여 닫는다.
   - scale set은 남긴다(R22).

**재시도 규칙** (R24, R41)

| 호출 위치 | 일시 오류(전송 오류, 429, 5xx, 409 세션 충돌) | 영구 오류 |
|---|---|---|
| 시작 단계(Docker 준비, scale set, 세션) | 1~60초 지수 백오프, 성공할 때까지 | 로그 후 종료 코드 1(launchd가 재시작) |
| `Scale()` 안의 runner 시작 | 재시도 없음, 다음 poll에서 수렴 | 같음 |
| 감시(컨테이너 삭제, `RemoveRunner`) | 백오프 최대 10번 | 로그, 다음 시작 때 R17 |
| broker(workflow run 조회, `AssumeRole`) | 재시도 없음, 해당 요청 거부 | 같음 |

라이브러리 자체의 HTTP 재시도(최대 4번)는 각 호출 안에서 그대로 쓴다.

**자격 중개** (R8~R15, R38~R40)
1. 스케일러는 runner를 만들 때 32바이트 임의 토큰을 만들어 그 runner 이름에 묶고, stdin으로 컨테이너에 넘긴다. 토큰은 거부되거나 컨테이너가 끝나면 폐기한다(R14, R38).
2. hook이 토큰과 runner 이름으로 요청하면 broker는 다음 순서로 처리한다.
   - 먼저 토큰을 검증한다(R40).
   - 그 runner의 `JobStarted` 메시지를 최대 90초 기다린다. `JobStarted`는 최대 약 50초 늦게 올 수 있다(확인함: actions/scaleset 이슈 #122 "GetMessage long poll does not wake for JobStarted", open).
   - R9의 다섯 조건을 확인한다. 메시지는 GitHub 서버가 보낸 것이고 workflow run은 GitHub REST에서 읽으므로, 컨테이너 안의 job 코드가 바꿀 수 없다.
   - `head_repository`와 `path`까지 확인하는 이유:
     - fork의 브랜치 이름이 `main`이어도 대상 저장소가 아니면 거른다.
     - 다른 브랜치가 main의 reusable workflow를 부르면 `JobWorkflowRef`만으로는 `@refs/heads/main`처럼 보일 수 있다. 그 경우 run의 `path`와 `head_branch`로 거른다.
     - `allowed-events`의 허용 범위를 `push`, `workflow_dispatch`, `schedule`로 제한해, `pull_request_target`이나 `workflow_run`처럼 외부 입력으로 main 코드가 도는 이벤트를 막는다.
3. 승인되면 broker는 다음을 한다.
   - 호스트의 `credentials.aws-profile`로 `credentials.role-arn`을 `AssumeRole`한다. `RoleSessionName`은 runner 이름이고, 세션 시간은 `credentials.session-duration`(기본 60분)이다(R15).
   - 그 자격과 `credentials.cluster-name`, `credentials.region`, `credentials.cluster-server`, CA, `credentials.namespace`를 돌려준다.
   - 역할의 최대 세션 시간은 1시간이다(확인함: 2026-11-20, 3600초 성공, 7200초 ValidationError).
   - 클러스터 주소와 CA는 설정으로 받는다. 역할 세션으로는 `describe-cluster`가 거부된다(확인함: 2026-11-20 시험, AccessDeniedException).
4. 갱신(R13):
   - AWS CLI가 `credential_process`로 다시 요청하면, broker는 그 runner의 자격을 만료 5분 전까지 캐시해서 돌려주고 그 뒤에는 새로 `AssumeRole`한다.
   - 승인된 runner의 컨테이너가 살아 있는 동안만 준다.
   - 스케일러가 재시작되면 토큰과 승인 상태가 사라지고, R16이 이전 컨테이너를 지우므로 진행 중이던 job은 실패한다.
5. 거부되면 broker는 403과 이유를 돌려주고, hook은 이유를 출력한 뒤 1로 끝난다(R10, R11). `JobWorkflowRef`나 run 정보를 해석할 수 없을 때도 거부한다.
   - 실제 job 메시지에 들어오는 `JobWorkflowRef`와 `WorkflowRunID` 값은 구현 단계의 첫 실제 시험에서 확인한다(사용자 결정). 두 필드는 `JobMessageBase`에 있고(확인함: actions/scaleset v0.5.0 `types.go`), ARC 테스트 데이터의 형식은 `owner/repo/.github/workflows/x.yml@refs/heads/main`이다(확인함: actions-runner-controller `cmd/ghalistener/metrics/metrics_integration_test.go`). 실제 값이 다르면 이 규칙대로 거부되므로 자격이 잘못 나가지 않고, 시험에서 R8이 실패해 드러난다.
6. 대기 runner는 job이 오기 전까지 자격을 갖지 않으므로 대기 시간과 자격 만료가 상관없다(R12).

**받아들인 trade-off** (사용자 결정)
- 자격 확인이 `JobStarted`를 기다리므로 job의 첫 step이 최대 90초 늦어질 수 있다. R4는 job 시작까지만 잰다.
- 호스트의 장기 AWS 키는 스케일러가 AssumeRole에만 쓰고 컨테이너에는 넣지 않는다. 호스트가 뚫리면 노출되는 위험은 받아들인다. 지금도 같은 키가 같은 호스트에 있다.
- actions/scaleset은 v0.5.0으로 고정하고, `WithHTTPClient`로 HTTP/2 ping(read-idle 30초, 응답 대기 10초)을 설정한 transport를 넣는다. v0.5.0 기본 transport는 끊긴 HTTP/2 연결에서 약 25분 멈출 수 있고, 그 수정은 아직 릴리스되지 않았다(확인함: actions/scaleset 이슈 #131 closed, 수정 #142는 v0.5.0 이후 main에만 있음).
- GitHub App은 AI, BE, FE 세 저장소에 설치되어 있고 Administration 쓰기 권한이 있다. 스케일러가 쓰는 것은 `github.url`의 저장소 하나다.
- 스케일러 재시작 중에는 진행 중이던 job이 실패한다(재시작은 비정상 상황이다).

**외부 계약: 설정 파일** (TOML, 모르는 키 거부, 비밀 값은 경로로만 참조)

| 키 | 기본값 | 허용 범위 / 설명 |
|---|---|---|
| `github.url` | (필수) | `https://github.com/<owner>/<repo>` 형식. R9의 대상 저장소 |
| `github.app-client-id`, `github.app-installation-id` | (필수) | 비어 있지 않은 값, 양의 정수 |
| `github.app-private-key-file` | (필수) | 있는 파일, 권한 0600 이하 |
| `scaleset.name` | `linux-arm64-deploy` | 소문자, 숫자, `-` |
| `runners.max` | 2 | 1 이상, 상한 없음 |
| `runners.idle` | 0 | 0 이상, `runners.max` 이하 |
| `runners.cpus` | 1 | 0보다 큼, 상한 없음 |
| `runners.memory` | `2g` | 256m 이상, 상한 없음. 스왑 없음 |
| `runners.pids` | 512 | 64 이상 |
| `runners.image` | (필수) | 로컬 이미지 ID `sha256:…` |
| `credentials.allowed-workflows` | (필수) | 1개 이상, `.github/workflows/<파일>` |
| `credentials.allowed-events` | `["push", "workflow_dispatch"]` | `push`, `workflow_dispatch`, `schedule` 중 1개 이상 |
| `credentials.aws-profile`, `credentials.role-arn` | (필수) | 호스트의 프로필, 역할 ARN |
| `credentials.session-duration` | `60m` | 15m 이상 60m 이하 |
| `credentials.cluster-name`, `credentials.region` | (필수) | `aws eks get-token`에 쓸 값 |
| `credentials.cluster-server`, `credentials.cluster-ca-file`, `credentials.namespace` | (필수) | kubeconfig에 쓸 값 |
| `broker.listen` | `127.0.0.1:8765` | loopback 주소만 |
| `shutdown.drain-timeout` | `10m` | 0보다 큼 |

설정 형식은 TOML이다. 주석을 달 수 있고 들여쓰기 실수가 적으며, runscaler도 같은 형식을 쓴다. 저장소에는 자리표시자만 넣은 예시 파일을 둔다.

**외부 계약: 운영**
- 로그: stderr에 JSON(`log/slog`)으로 쓰고, LaunchAgent가 파일로 남긴다(R31).
- 서비스: `service install`이 LaunchAgent를 설치한다(R44).
  - 설정: `KeepAlive`, `ThrottleInterval` 10초, `ExitTimeOut`은 drain-timeout + 60초.
  - launchd는 작업을 멈출 때 SIGTERM을 보내고 `ExitTimeOut`만큼 기다린 뒤 SIGKILL을 보낸다(확인함: `man launchd.plist`).
  - 시작할 때 Docker가 준비될 때까지 R24의 백오프로 기다린다(R19).
  - 호스트는 자동 로그인과 Docker Desktop 로그인 시 자동 시작을 켜 두었다(확인함: 2026-11-20 설정 조회).
- 멈추기: `launchctl bootout gui/$(id -u)/<label>`. SIGTERM이 가므로 R20이 적용된다.

**테스트**
- 단위 테스트는 가짜 scaleset 클라이언트, 가짜 GitHub REST, 가짜 Docker, 가짜 STS로 한다(`go test ./...`).
- 실제 GitHub, Docker, 클러스터를 쓰는 시험은 `integration` 빌드 태그로 나누고, 사용자 승인 후에만 돌린다.
- R19는 두 시나리오로 측정한다: 스케일러 `kill -9` 뒤, 그리고 호스트 재부팅 뒤 시험 job이 시작되기까지의 시간.

## How each constraint in the intent is met

| Constraint | Met by |
| :---- | :---- |
| 클러스터 접속 제한을 우회하지 않고, 허용된 네트워크의 호스트에서 되는 경로만 쓴다 | 설계: 호스트에서 AssumeRole, 컨테이너는 호스트 네트워크 출구 사용. 접속 제한을 바꾸거나 피하는 구성 요소 없음 |
| 클러스터 안 runner(ARC 등)는 쓸 수 없다 | 설계: 호스트 Docker 컨테이너만 사용 |
| Go와 `actions/scaleset` | 설계: scaleset v0.5.0 고정 |
| GitHub App, 저장소 단위 등록(백엔드부터) | 설계: `NewClientWithGitHubApp`, `github.url`은 저장소 URL. R22 |
| `runs-on` = `linux-arm64-deploy`, 추가 라벨 없음 | 설계: `scaleset.name` 기본값, `Labels` 비움 |
| 기본값(최대 2, 대기 0, CPU 1, 메모리 2GB), 설정으로 늘림, 코드 상한 없음 | 설정 표, R1, R7, R28, R29 |
| 컨테이너 격리와 임시 자격만 | R12, R15, R32~R36, R39 |
| 배포 job은 `environment: prod`를 쓰지만 Environment만으로는 자격이 막히지 않는다 | R9~R11, R38, R40, 설계: 자격 중개 |
| 호스트 상시 가동, 재부팅 뒤 자동 로그인과 Docker 시작 | R19. 호스트의 자동 로그인과 Docker Desktop 자동 시작을 켰다(확인함: 2026-11-20 설정 조회) |
| 공개 저장소, 라이선스 없음, 내부 이름과 비밀 값 금지 | R31, 설정 예시에는 자리표시자만 |
| 커밋, 푸시, GitHub와 Docker와 클러스터 시험은 승인 후 | 설계: `integration` 태그 시험은 승인 후 실행 |

## Open questions from the intent

- 없음. INTENT의 Open questions가 비어 있다.

## Areas of concern

없음. 실제 job 메시지의 `JobWorkflowRef`와 `WorkflowRunID` 값 확인은 Design의 자격 중개 5번 결정대로 구현 단계의 첫 실제 시험으로 넘겼다.
