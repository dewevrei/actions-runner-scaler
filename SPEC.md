# Spec: 배포 job용 self-hosted runner 자동 확장

From: INTENT.md (dewevrei, 5d00689). Status: accepted.
Skills applied: actions-scaleset

## Requirements

스케일링 (R1~R7, R37, R61, R80, R87, R93~R95)
- R1. 스케일러는 pool마다 목표 runner 수를 `min(max, idle + 배정된 job 수)`로 계산한다. `max`와 `idle`은 그 pool의 설정값이고, 배정된 job 수는 그 pool의 scale set에 GitHub가 보내는 `statistics.TotalAssignedJobs`다.
- R2. 존재하는 runner 수가 목표보다 적으면 스케일러는 그 차이만큼 runner 컨테이너를 시작한다.
- R3. pool의 runner 컨테이너 수는 그 pool의 `max`를 넘지 않는다.
- R4. pool의 runner가 `max`보다 적고 두 이미지가 로컬에 있을 때, 스케일러가 job의 첫 메시지(`JobAvailable` 또는 `JobAssigned`)를 받은 시각부터 GitHub job 기록의 `started_at`까지 60초를 넘지 않는다(R87의 경우는 뺀다).
- R5. runner 컨테이너는 job 하나를 끝내면 종료된다.
- R6. runner 컨테이너가 종료되면 스케일러는 그 runner 자원을 지우고 그 runner의 GitHub 등록을 제거한다.
- R7. 배정된 job이 없으면 pool의 runner 컨테이너 수는 그 pool의 `idle`과 같다. 단, `RemoveRunner`가 실패한 runner(R37)는 남을 수 있다.
- R37. 존재하는 runner가 목표보다 많으면, 스케일러는 `RemoveRunner`가 성공한 runner의 컨테이너만 지우고, 실패한 runner는 대기 runner로 남긴다.
- R61. runner 시작이 3번 연속 실패하면 다음 시작 전에 백오프만큼 기다린다. 백오프는 10초에서 시작해 실패할 때마다 두 배가 되고 5분을 넘지 않으며, 첫 시작 성공에서 처음으로 되돌아간다.
- R80. 스케일러는 scale set을 만들 때 `RunnerSetting.DisableUpdate`를 true로 둔다. 기존 scale set의 값이 false면 경고 로그를 남긴다.
- R87. R4의 60초는 속도 제한 대기(R52) 중, 시작 실패 백오프(R61) 중, 그 job을 위한 runner 시작이 일시 오류로 한 번 실패한 경우에는 적용하지 않는다.
- R93. R37에서 `RemoveRunner`가 일시 오류로 실패하면, 다음 `Scale`에서도 목표보다 많을 때 다시 시도한다.
- R94. R37에서 `RemoveRunner`가 영구 오류로 실패하면 다시 시도하지 않고 로그와 마지막 오류에 남긴다. 오류 drain은 시작하지 않는다.
- R95. R37의 제거 중 속도 제한 오류를 받으면 그 `Scale`의 나머지 제거를 멈춘다.

판정 (R8~R15, R38~R40, R65, R66, R81~R83)
- R8. 시험 저장소 main의 허용된 job은 runner 안에서 GitHub OIDC로 관리자 안내문의 Role을 받아, 안내문의 Namespace에서 Pod 목록 조회에 성공한다.
- R9. 스케일러는 runner의 `JobStarted`를 받으면 hook 요청과 관계없이 판정을 내린다. 다음 아홉 조건을 모두 만족할 때만 허용하고, 하나라도 어기거나 값이 없거나 해석할 수 없으면 거부한다.
  - `JobStarted`의 `OwnerName`/`RepositoryName`이 pool의 저장소다.
  - `JobWorkflowRef`의 저장소 부분이 pool의 저장소이고, 경로 부분이 `allowed-workflows`에 있다.
  - `JobWorkflowRef`의 ref 부분이 정확히 `refs/heads/main`이다.
  - `JobStarted`의 `EventName`이 pool의 `allowed-events`에 있다.
  - `WorkflowRunID`로 조회한 workflow run의 `head_repository.full_name`이 pool의 저장소이고 `head_branch`가 `main`이다.
  - 그 run의 `event`가 `allowed-events`에 있다.
  - 그 run의 `path`가 `allowed-workflows`에 있다.
  - `JobWorkflowRef`의 경로 부분이 그 run의 `path`와 같다.
  - 그 run의 `referenced_workflows`가 빈 배열이다.
- R10. 거부된 job의 결과는 실패이고, `if: always()`나 `failure()` 조건의 step을 포함해 workflow step이 하나도 실행되지 않는다.
- R11. 거부 판정을 내리면 스케일러는 판정을 hook에 보이기 전에 그 runner 자원 삭제를 시작하고, 판정부터 10초 안에 runner 컨테이너가 없어진다.
- R12. 스케일러는 runner와 BuildKit 컨테이너에 AWS 자격, 클러스터 자격, AWS 설정 파일, kubeconfig, GitHub App 개인키, 설치 토큰을 넣지 않는다(대기 runner 포함).
- R13. (dropped) 스케일러가 AWS 자격을 발급하거나 갱신하지 않는다. Role 세션 시간은 Design의 가정.
- R14. job 확인 토큰은 발급받은 runner 이름에 묶인다. 다른 runner 이름으로 쓰거나 폐기된 뒤 쓰면 401로 거부된다.
- R15. (dropped) 스케일러가 AWS 자격을 다루지 않으므로 R12로 합쳤다.
- R38. 스케일러는 그 runner 컨테이너가 끝나면 토큰을 폐기한다. 토큰이 살아 있는 동안 판정 조회는 몇 번이든 같은 판정을 돌려준다.
- R39. job 확인 토큰은 job step의 환경 변수로 상속되지 않고, 허용 판정 뒤 runner 안의 토큰 파일은 없다.
- R40. job 확인 서버는 토큰 검증에 실패한 조회에 바로 401을 돌려주고, 그 runner의 busy 시각(R81)을 기록하지 않는다.
- R65. 판정 중 workflow run 조회가 실패하면(일시 오류와 영구 오류 모두) 다시 시도하지 않고 그 job만 거부한다. 오류 drain은 시작하지 않는다.
- R66. job 시작 hook은 판정을 조회만 하고, 허용을 받을 때만 0으로 끝난다. 연결 실패, 401, `pending`이면 끝내지 않고 2초마다 다시 조회하며, 거부를 받거나 hook 시작부터 240초가 지나면 1로 끝난다.
- R81. 스케일러는 runner마다 타이머를 두고, busy가 된 때부터 정확히 180초가 지날 때까지 허용 판정이 없으면 그 runner를 거부한 것으로 보고 R11대로 지운다. busy가 된 때는 토큰이 맞는 첫 hook 조회, `JobStarted`, R53의 `JobStillRunningError`, R83의 발견 중 가장 이른 때다.
- R82. hook은 job이 바꿀 수 있는 환경 변수와 `PATH`의 영향을 받지 않고, 스케일러가 컨테이너 시작 때 준 주소와 runner 이름으로만 판정을 조회한다.
- R83. R42의 대조는 허용 판정이 없는 runner 컨테이너의 프로세스 목록에 `Runner.Worker`가 있으면 그 runner를 busy로 표시한다.

수명주기와 drain (R16~R23, R25, R42, R53~R55, R62, R63, R67, R88~R92)
- R16. 스케일러는 시작할 때 `managed-by` 라벨이 붙은 이전 runner 자원을 모두 지운다. 이전 실행에서 진행 중이던 job은 이때 실패한다.
- R17. 스케일러는 시작할 때 pool마다 저장소 runner 목록에서, 이름이 `<pool 이름>-`로 시작하고 `GetRunnerByName`의 `RunnerScaleSetID`가 그 pool의 scale set ID와 같으며 살아 있는 runner 컨테이너가 없는 등록을 제거한다. 목록 조회의 일시 오류는 최대 3번 다시 시도하고, 그래도 실패하거나 영구 오류면 로그를 남기고 정리를 건너뛴다(시작을 막지 않는다).
- R18. JIT 발급 뒤 네트워크, BuildKit 컨테이너, runner 컨테이너 중 하나의 생성이나 시작이 실패하면 스케일러는 이미 만든 runner 자원을 지우고 그 runner 등록을 바로 제거한다.
- R19. 스케일러가 비정상 종료되거나 호스트가 재시작되면, 사람 개입 없이 5분 안에 스케일러가 다시 job을 받는다. 오류 대기(R46)와, 원인이 남아 있는 동안의 종료 코드 1(R60, R78)은 제외한다.
- R20. 첫 세션을 만든 뒤 정상 동작 중(오류 drain과 오류 대기가 아님)에 SIGTERM이나 SIGINT를 처음 받으면, 세션 재생성 백오프 중이라도 스케일러는 신호 drain을 시작하고 새 runner를 만들지 않는다.
- R21. 신호 drain 중에 두 번째 SIGTERM이나 SIGINT를 받거나, 오류 drain 중에 SIGTERM이나 SIGINT를 받으면, 기다리지 않고 남은 runner 자원을 지우고 `RemoveRunner`를 1회 시도한 뒤 종료한다.
- R22. pool마다 scale set이 없으면 만들고, 있으면 다시 쓴다. 스케일러가 종료돼도 scale set은 지우지 않는다.
- R23. `delete-scaleset <pool 이름>` 명령은 그 pool의 scale set과 그 runner 등록을 지운다.
- R25. `listener.Run`이 취소가 아닌 일시 오류나 세션 없어짐으로 끝나면 스케일러는 새 세션을 만들어 메시지 수신을 다시 시작한다. 세션 없어짐은 메시지 세션 호출(`GetMessage`, `DeleteMessage`, `AcquireJobs`, 세션 갱신)이 404(`NotFoundError`)로 실패한 경우다.
- R42. 스케일러는 runner 컨테이너마다 종료를 기다리고, 30초마다 실제 runner 자원 목록과 내부 runner 상태를 맞춘다. 짝이 없는 BuildKit 컨테이너와 네트워크는 지우고, 영구 오류로 삭제에 실패한 대상은 다시 시도하지 않는다.
- R53. 오류 drain 중 대기 runner는 R37로 지운다. `RemoveRunner`가 `JobStillRunningError`를 돌려주면 그 runner를 busy로 표시하고 R55의 대기에 넣는다. 그 밖의 이유로 `RemoveRunner`가 실패한 대기 runner는 컨테이너를 지우고, 등록은 다음 시작의 R17이 정리한다.
- R54. 오류 drain 중 세션을 계속 쓸 수 있으면 세션과 job 확인 서버를 유지한다. 세션을 쓸 수 없으면 새 job은 `JobStarted`를 받지 못해 R81로 지워진다. 이미 허용된 job은 계속 돈다.
- R55. 오류 drain 중 실행 중인 job의 컨테이너가 모두 끝나거나 `shutdown.drain-timeout`이 지나면, 남은 컨테이너를 지우고 `RemoveRunner`를 1회 시도한 뒤 오류 대기에 들어간다.
- R62. runner 컨테이너, BuildKit 컨테이너, 전용 네트워크, 익명 볼륨은 함께 만들고 함께 지운다.
- R63. 스케일러는 전용 네트워크와 BuildKit 컨테이너를 만들고 시작한 뒤에 runner 컨테이너를 만든다. buildkitd의 준비 완료는 기다리지 않는다.
- R67. R42의 대조에서 대기 runner의 BuildKit 컨테이너가 없거나 멈춰 있으면 스케일러는 그 runner를 R37의 규칙으로 지운다.
- R88. 신호 drain 중에는 `listener.Run`이 끝나도 새 세션을 만들지 않는다.
- R89. drain 중(신호 drain과 오류 drain)에도 `AcquireJobs`는 계속 부른다. 받은 job은 재시작 뒤 `TotalAssignedJobs`로 세어져 그때 runner를 받는다.
- R90. drain 중에도 `maxCapacity`는 pool의 `max` 그대로 둔다.
- R91. 신호 drain 중 대기 runner는 R37의 규칙으로 지운다.
- R92. 신호 drain은 실행 중인 job이 끝날 때까지(최대 `shutdown.drain-timeout`) 세션과 job 확인 서버를 유지하고, 그 뒤 남은 runner 자원을 지우고 `RemoveRunner`를 1회 시도한 뒤 종료한다.

오류 규칙과 종료 (R24, R41, R46~R48, R50~R52, R57, R60, R64, R78, R84, R85, R96~R100)
- R24. 시작 단계의 호출(Docker 준비 확인, `runner.image`와 `buildkit.image`의 로컬 존재 확인, R16의 Docker 목록 조회, pool마다 scale set get-or-create, `SetSystemInfo`, 첫 세션 생성)이 일시 오류로 실패하면, 1초에서 시작해 최대 60초 간격의 지수 백오프로 성공할 때까지 다시 시도한다. 일시 오류는 전송 오류, 5xx, 속도 제한 오류(429, 속도 제한 403), 409 세션 충돌, R50의 Docker 삭제 오류다.
- R41. GitHub와 Docker 호출의 4xx 응답 중 속도 제한 오류, 세션 생성의 409, 세션 없어짐(R25), R50의 경우를 뺀 나머지(세션 생성의 404와 세션 갱신 뒤의 401 포함)는 영구 오류로 보고, 같은 호출을 자동으로 다시 시도하지 않는다.
- R46. 첫 세션을 만들기 전에 설정 오류(R28, R30, R68), Docker 데몬 TCP 노출(R79), 또는 시작 단계 호출(Docker 준비 확인, 이미지 두 개의 로컬 존재 확인, R16 목록 조회, scale set get-or-create, `SetSystemInfo`, job 확인 서버 listen, 첫 세션 생성)의 영구 오류가 나면, 스케일러는 종료하지 않고 오류 대기 상태로 들어간다.
- R47. 오류 대기 상태에서 스케일러는 GitHub와 Docker를 호출하지 않고, 자동으로 다시 시도하지 않는다. 운영자가 원인을 고친 뒤 재시작해야 벗어난다.
- R48. 오류 대기 상태에서 SIGTERM이나 SIGINT를 받으면 바로 종료한다.
- R50. `RemoveRunner`의 `RunnerNotFoundError`와 일반 `NotFoundError`(AgentNotFoundException이 아닌 404)는 성공으로 보고, `JobStillRunningError`는 일시 오류로 본다. Docker의 컨테이너, 네트워크, 볼륨 삭제에서 not found는 성공으로, 409와 403은 일시 오류로 본다.
- R51. 실행 중 메시지 세션 호출(`GetMessage`, `DeleteMessage`, 세션 갱신, 세션 재생성), scale set 호출, `AcquireJobs`가 영구 오류로 실패하면(그 호출 안의 GitHub App 인증 오류 포함) 스케일러는 새 runner를 만들지 않는다(오류 drain 시작).
- R52. 속도 제한 오류를 받으면, 응답의 `Retry-After`나 제한 해제 시각까지와 백오프 간격 중 긴 쪽이 지나기 전에는 같은 호출을 다시 하지 않는다. 응답에 `Retry-After`나 해제 시각이 없으면 최소 60초를 기다린다. 판정 중의 workflow run 조회는 예외로 R65를 따른다.
- R57. 첫 세션을 만들기 전(백오프 대기 포함)에 SIGTERM이나 SIGINT를 받으면 바로 종료한다.
- R60. 잠금(R78)을 잡은 프로세스는 status socket을 listen하기 전에 남아 있는 socket 파일을 지우고, 그래도 listen하지 못하면 로그를 남기고 종료 코드 1로 끝난다.
- R64. runner 하나의 생성 실패(JIT 발급, 네트워크, BuildKit 컨테이너, runner 컨테이너의 생성과 시작, 일시 오류와 영구 오류 모두)는 R18의 정리 뒤 R61의 시작 실패로만 센다. 오류 drain을 시작하지 않는다.
- R78. 스케일러는 시작할 때 단일 실행 잠금을 잡는다. 잠금을 다른 프로세스가 잡고 있으면 로그를 남기고 종료 코드 1로 끝난다.
- R84. 신호로 인한 종료는 모두 종료 코드 0이다: 신호 drain의 끝(R92, drain-timeout 포함), R21, R48, R57.
- R85. 그 밖의 모든 종료는 0이 아닌 종료 코드다(R60, R78, 예상하지 못한 오류).
- R96. R41의 예외로, runner 하나의 생성 영구 오류(R64)는 R61의 백오프 아래에서 다음 poll에 다시 시도된다.
- R97. 영구 오류의 처리는 상태 코드가 아니라 호출 위치로만 정한다. JIT 발급의 401은 R64를, 축소의 `RemoveRunner`의 401은 R94를 따르고 R51을 따르지 않는다.
- R98. 실행 중 R51에 없는 호출(JIT, 컨테이너·네트워크·볼륨, `RemoveRunner`, workflow run 조회)의 영구 오류는 인증 오류라도 오류 drain을 시작하지 않는다.
- R99. 세션 닫기의 오류는 로그만 남긴다.
- R100. 신호 drain 중의 영구 오류는 로그만 남기고 오류 drain으로 바꾸지 않으며, 신호 drain이 끝나면 오류 대기에 들어가지 않고 종료한다.

운영과 설정 (R26~R31, R43~R45, R49, R56, R58, R68, R79, R86)
- R26. 첫 세션을 만든 뒤 정상 동작 중(drain도 오류 대기도 아님)이면 `status` 명령은 pool마다 실행 중 runner 수와 대기 runner 수(시작 중인 runner 포함), 그리고 마지막 오류와 그 시각을 출력하고 0으로 끝난다.
- R27. 스케일러가 실행 중이 아니면 `status` 명령은 "실행 중 아님"을 출력하고 1로 끝난다.
- R28. 설정 파일에 모르는 키, 형식 오류, 허용 범위 밖의 값이 있으면 스케일러는 오류 대기에 들어가고, 발견한 오류를 모두 한 번에 로그와 `status`에 보인다. 허용 범위는 설정 표에 있다.
- R29. 모든 pool의 `max × (runner-memory + build-memory)` 합이 Docker가 쓸 수 있는 메모리를 넘으면 시작 로그에 경고를 남기고 계속 실행한다.
- R30. GitHub App 개인키 파일의 권한이 0600보다 넓으면 R28과 같이 오류 대기에 들어간다.
- R31. 로그, 오류 메시지, `status` 출력에 JIT 설정, GitHub 토큰, 개인키, job 확인 토큰의 값이 나타나지 않는다.
- R43. 상태 조회는 소유자만 읽을 수 있는 unix socket으로만 제공하고, 컨테이너에서 닿는 job 확인 서버 주소로는 제공하지 않는다.
- R44. `service install`은 로그인 때 스케일러를 띄우는 LaunchAgent를 설치하고 `service uninstall`은 지운다. 그 LaunchAgent로 launchd는 신호 종료(0) 뒤에는 다시 띄우지 않고, 그 밖의 종료 뒤에는 60초에 한 번보다 자주 않게 다시 띄우며, 멈출 때 `shutdown.drain-timeout` + 60초를 기다린 뒤 강제 종료한다.
- R45. 오류 대기 상태에서 `status` 명령은 "오류 대기", 오류 내용과 시각을 출력하고 1로 끝난다.
- R49. `delete-scaleset` 명령은 단일 실행 잠금(R78)이 잡혀 있으면(오류 대기 포함) 아무것도 지우지 않고 1로 끝난다.
- R56. drain 중(신호 drain이나 오류 drain)에는 `status` 명령이 "drain 중", pool마다 실행 중 runner 수와 대기 runner 수, 마지막 오류와 시각을 출력하고 0으로 끝난다.
- R58. 첫 세션을 만들기 전(시작 중, 백오프 포함)에는 `status` 명령이 "시작 중"과 마지막 오류와 시각을 출력하고 0으로 끝난다.
- R68. 설정의 pool이 1개가 아니면(0개나 2개 이상) R28과 같이 오류 대기에 들어가고 "이번 버전은 pool 1개만 지원한다"를 보인다.
- R79. 시작할 때 Docker 데몬이 호스트의 TCP 2375에서 인증 없이 응답하면 스케일러는 오류 대기에 들어가고 "Docker 데몬이 TCP 2375로 노출됨"을 보인다.
- R86. `status` 명령은 pool마다 연속 시작 실패 횟수(R61)와 마지막 시작 오류와 그 시각을 보인다. 횟수가 0이 아니어도 상태는 정상이고 0으로 끝난다.

runner 컨테이너 (R32~R36, R59, R69~R71)
- R32. runner 컨테이너는 비root 사용자로 돌고, privileged가 아니며, 모든 capability를 제거하고, no-new-privileges가 켜져 있다.
- R33. runner 컨테이너에는 호스트 Docker 소켓을 포함한 호스트 경로가 마운트되지 않는다.
- R34. runner 컨테이너의 CPU, 메모리, 프로세스 수 제한은 그 pool의 `runner-cpus`, `runner-memory`, `runner-pids`와 같고, 메모리 스왑은 없다.
- R35. runner 컨테이너와 BuildKit 컨테이너는 linux/arm64 이미지로 만들어진다.
- R36. JIT 설정과 job 확인 토큰은 컨테이너의 환경 변수, 명령 인자, 라벨(`docker inspect`로 보이는 값)에 나타나지 않는다.
- R59. job step의 환경 변수에 JIT 설정(`ACTIONS_RUNNER_INPUT_JITCONFIG`)이 없다.
- R69. runner 이미지에는 docker CLI, buildx, aws CLI v2, kubectl, git(모두 arm64)이 있고, Docker 데몬(dockerd) 실행 파일은 없다.
- R70. 스케일러가 runner 컨테이너에 넣는 환경 변수는 `SCALER_JOBCHECK_URL`, `SCALER_RUNNER_NAME`, `BUILDKIT_HOST` 세 개뿐이다.
- R71. runner 안의 job이 builder를 지정하지 않고 `docker buildx build`를 부르면 그 runner의 BuildKit 컨테이너가 빌드한다.

빌드 (R72~R77)
- R72. BuildKit 컨테이너는 privileged가 아니고, capability는 모두 제거한 뒤 `SETUID`와 `SETGID`만 더하며, 보안 옵션 중 기본값에서 바꾼 것은 `seccomp=unconfined` 하나뿐이다. runner 컨테이너에는 seccomp 예외가 없다.
- R73. BuildKit 컨테이너는 비root 사용자로 돌고, 호스트 경로(호스트 Docker 소켓 포함)를 마운트하지 않으며, 호스트에 포트를 공개하지 않는다.
- R74. BuildKit 컨테이너의 CPU, 메모리, 프로세스 수 제한은 그 pool의 `build-cpus`, `build-memory`, `build-pids`와 같고, 메모리 스왑은 없다.
- R75. runner와 그 BuildKit 컨테이너는 그 runner의 전용 네트워크에만 연결된다. 다른 runner 컨테이너에서 이 BuildKit의 1234 포트로 TCP 연결하면 실패한다.
- R76. BuildKit 컨테이너의 볼륨은 이미지가 선언한 `/home/user/.local/share/buildkit`의 익명 볼륨 하나뿐이고(이름 있는 볼륨과 호스트 경로 없음), 컨테이너와 함께 지워진다. 새 runner의 BuildKit은 빈 캐시로 시작한다.
- R77. 시험 저장소 main의 허용된 job은 `docker buildx build --push`로 몇 MB 크기의 이미지를 빌드해 레지스트리에 push하는 데 성공한다.

## Design

용어: pool은 설정의 `[[pools]]` 항목 하나이고 scale set 하나에 대응한다. runner 자원은 runner 하나의 runner 컨테이너, BuildKit 컨테이너와 그 익명 볼륨, 전용 Docker 네트워크다. 판정은 스케일러가 runner의 job마다 내리는 허용 또는 거부다.

**구성 요소**
- `cmd/actions-runner-scaler`: 하위 명령 `run`(서비스), `status`, `delete-scaleset <pool 이름>`, `service install|uninstall`, `version`.
- GitHub 연동: `actions/scaleset` v0.5.0의 `NewClientWithGitHubApp`을 pool마다 하나(`GitHubConfigURL`은 pool의 `repo-url`) 만들고 App 자격은 전역 하나를 같이 쓴다. GitHub REST는 같은 App의 설치 토큰으로 workflow run 조회(`GET /repos/{owner}/{repo}/actions/runs/{run_id}`)와 저장소 runner 목록(`GET /repos/{owner}/{repo}/actions/runners`, R17)을 부른다. 모두 쓰는 메서드만 가진 좁은 인터페이스 뒤에 둔다.
  - Administration(읽기 쓰기)으로 scale set 생성, 세션, JIT 발급, runner 제거가 되고, Actions(읽기)로 run 조회가 된다(확인함: 2026-11-20 App 시험). App 토큰으로 scale set 삭제와 runner 목록이 되는지는 미확인이다(Areas of concern 14).
- Docker 연동: Docker Engine API Go 클라이언트를 인터페이스 뒤에 두고 컨테이너, 네트워크, 볼륨의 생성, 삭제, 목록, 대기, 프로세스 목록을 쓴다.
- pool: pool마다 scale set, 세션, listener를 따로 갖고, runner 표(runner 이름 → pool, 토큰, 판정, busy 시각), Docker 클라이언트, job 확인 서버, status, App 자격은 함께 쓴다. 이번 버전은 pool 1개만 받는다(R68). 한 pool의 영구 오류가 다른 pool에 어디까지 미칠지 정하지 않았기 때문이다(Areas of concern 2). 나중에 R68의 범위만 넓히면 되므로 기존 설정 파일은 그대로 유효하다.
- job 확인 서버: 스케일러 안에서 `jobcheck.listen`(기본 `127.0.0.1:8765`)의 HTTP 서버로 돌고 판정 조회에만 답한다. 컨테이너는 `host.docker.internal`로 접속한다. 기본 네트워크에서 호스트 loopback 서비스로 HTTP 200이 왔고(확인함: 2026-11-20), 사용자 정의 네트워크에서는 이름이 192.168.65.254로 풀린다(확인함: 2026-11-24 로컬 시험). 실제 접속은 미확인이다(Areas of concern 10).
- status는 `~/.local/state/actions-runner-scaler/status.sock`(0600)으로, 잠금은 같은 폴더의 `lock`(flock)으로 한다. 시작 순서는 잠금(R78) → status socket(R60) → 설정 검증 → Docker 노출 확인(R79) → 이미지 확인 → 정리(R16, R17) → scale set → 세션이다. 설정 오류도 status로 보이도록 socket을 설정보다 먼저 연다. 잠금이 먼저이므로 R60이 지우는 socket 파일은 실행 중인 다른 프로세스의 것이 아니다.
- Docker 노출 확인(R79): `127.0.0.1:2375`에 TCP로 접속해 `GET /_ping`이 200과 `OK`면 노출로 본다. 접속이 거부되거나 다른 응답이면 계속한다. Docker Desktop의 "Expose daemon on tcp://localhost:2375 without TLS"를 켜면 컨테이너가 `host.docker.internal`로 호스트 Docker 전체를 제어할 수 있기 때문이다. 2026-11-24에는 닫혀 있었다(확인함: 로컬 시험).
- runner 이미지: `image/`의 Dockerfile로 호스트에서 빌드한다. 기반은 공식 `actions-runner`(linux/arm64, digest 고정)이고 aws CLI v2, kubectl, docker CLI, buildx를 버전을 고정해 더한다(R69). 공식 이미지에 curl, jq, unzip이 있다(확인함: 2026-11-20). 기반 이미지에 git, docker CLI, buildx, dockerd가 있는지는 미확인이고, 데몬 실행 파일이 있으면 지운다. 이미지는 로컬 이미지 ID(`sha256:…`)로 고정하고(확인함: 2026-11-20, 이미지 ID로 컨테이너 생성) pull하지 않는다.
  - entrypoint 래퍼: stdin의 첫 줄(JIT)을 `ACTIONS_RUNNER_INPUT_JITCONFIG`로, 둘째 줄(토큰)을 `/home/runner/.scaler/token`(0400)으로, `SCALER_JOBCHECK_URL`과 `SCALER_RUNNER_NAME`을 `/home/runner/.scaler/endpoint`(0400)로 넘긴다. `docker buildx create --driver remote --name scaler --use "$BUILDKIT_HOST"`로 기본 builder를 만든 뒤(R71) `run.sh`를 exec한다. JIT 한 줄 stdin 전달은 `docker inspect`에 나타나지 않았다(확인함: 2026-11-20 시험). 두 줄 stdin과 데몬 없는 `docker buildx create`는 미확인이다(Areas of concern 7).
  - job 시작 hook(`ACTIONS_RUNNER_HOOK_JOB_STARTED`): endpoint 파일의 주소로 `/usr/bin/curl -q --noproxy '*'`를 써서 판정을 조회하고, 허용이면 토큰 파일을 지운다(R66, R82). 환경 변수 대신 파일과 절대 경로를 쓰는 이유: actions/runner main은 hook을 빈 환경 사전에 `github`과 `runner` 컨텍스트 변수만 더해 만든다(확인함: main 소스 `JobHookProvider.cs` 68~77행, `ScriptHandler.cs` 291~299행). 고정할 runner 버전에서 같은지는 미확인이다(Areas of concern 11). hook은 `always()` 조건의 pre-job step이고 0이 아닌 종료 코드는 job을 실패시킨다(확인함: actions/runner `JobExtension.cs`, `docs/adrs/1751-runner-job-hooks.md`). hook은 판정 전에 step이 돌지 않게 붙잡는 장치이고, 강제는 R11, R81의 삭제가 한다.
- BuildKit 컨테이너: 이미지 `buildkit.image`(`moby/buildkit:<버전>-rootless@sha256:…`, 로컬, pull 안 함), 명령 `--addr tcp://0.0.0.0:1234 --oci-worker-no-process-sandbox`, `seccomp=unconfined`, `CapDrop ALL`과 `CapAdd SETUID, SETGID`(R72). 이름 `<runner 이름>-buildkit`, 네트워크 `<runner 이름>-net`, 라벨 `managed-by`, runner 이름, 종류. 삭제는 RemoveVolumes로 한다(R16, R42, R76). runner와 BuildKit 사이는 TLS 없는 TCP다. 전용 네트워크 안에서 그 runner만 닿기 때문이다.
  - 2026-11-24 로컬 시험(Docker Desktop 29.6.1, v0.34.2-rootless arm64 uid 1000)의 결과(확인함): `--oci-worker-no-process-sandbox`만이면 rootlesskit의 "fork/exec /proc/self/exe: operation not permitted"로 실패하고, `seccomp=unconfined`를 더하면 성공한다. `seccomp=unconfined`와 `systempaths=unconfined`도 성공하고, `seccomp=unconfined`만이면 `RUN`의 procfs mount에서 실패한다.
  - 같은 시험에서(확인함): capability는 기본값과 `--cap-drop ALL --cap-add SETUID --cap-add SETGID`(최소 집합)에서 성공하고, `--cap-drop ALL`만이면 newuidmap에서, no-new-privileges를 더하면 "newuidmap … Could not set caps"로 실패했다. 이미지는 `VOLUME /home/user/.local/share/buildkit`을 선언하고 익명 볼륨은 `docker rm -v`로 지워진다. 네트워크 B의 컨테이너는 네트워크 A의 BuildKit 이름을 풀지 못하고 IP로도 연결하지 못한다(R75).
  - `systempaths=unconfined` 대신 no-process-sandbox를 고른 이유: 빌드의 `RUN` 프로세스가 같은 컨테이너의 buildkitd를 보고 신호를 보내거나 메모리를 읽을 수 있지만 영향은 job 하나만 쓰는 컨테이너 안에 그친다. `systempaths=unconfined`는 Docker VM 커널의 `/proc`와 `/sys` 가림을 푼다.
- 빌드 경로: privileged인 dind와 호스트 Docker 소켓은 INTENT 제약상 쓸 수 없다. runner마다 BuildKit을 두면 캐시와 빌드 결과가 다른 job과 섞이지 않는다(R76). 관리자 안내문의 `docker build`와 `docker push`는 데몬이 필요하므로 workflow는 `docker buildx build --push`를 쓴다.
- DisableUpdate(R80): 일회용 runner가 시작마다 runner 프로그램을 바꾸면 시작이 늦어지고(R4) 고정한 이미지와 달라진다. 참조한 scaleset 기반 구현 세 곳이 모두 true로 만든다(확인함: 코드 조회).

**스케일링 흐름** (actions/scaleset v0.5.0의 `listener` 패키지 계약을 따른다. pool마다 한 벌씩 돈다)
1. 시작: JIT로 등록만 되고 컨테이너가 없는 runner는 저장소 runner 목록에 `offline`으로 나타나고, `GetRunnerByName`의 `RunnerScaleSetID`로 소속을 알 수 있다. 이미 지운 runner를 다시 지워도 오류가 없다(확인함: 2026-11-20 시험). 기본 runner group(id 1)에서 scale set get-or-create를 하고, `Labels`를 비우면 서버가 이름을 유일한 라벨로 붙인다(확인함: 같은 날).
2. `SetSystemInfo` 뒤 세션을 만든다. `ConflictError`(409)면 백오프 재시도(R24). 닫지 못한 세션은 1분 안에 만료된다(확인함: 2026-11-20, 30초에 409, 61초에 성공). `MaxRunners`를 낮추지 않는 이유(R90): 0일 때 서버 쪽 효과가 미확인이다(actions/scaleset 이슈 #133).
3. `Scale()`: nil과 초기 메시지(-1) 처리, `JobAvailable`마다 `AcquireJobs`, `JobStarted`마다 busy 표시와 판정 고루틴 시작(run 조회를 기다리지 않음), `JobCompleted` 처리, `TotalAssignedJobs`로 목표 수 계산, 부족하면 시작, 남으면 R37. 시작 실패는 로그만 남기고 nil을 반환한다(R64). 모르는 runner의 메시지는 로그만 남긴다.
4. runner 시작: `GenerateJitRunnerConfig`로 이름 `<pool 이름>-<8자 임의값>`의 JIT를 발급하고, 네트워크, BuildKit, runner 컨테이너 순으로 만들고(R63), attach한 stdin으로 JIT와 토큰을 쓰고 닫는다.
5. 감시: runner마다 `ContainerWait`로 종료를 기다리고 runner 자원 삭제, 토큰 폐기, `RemoveRunner`를 한다. 삭제의 일시 오류는 백오프로 최대 10번 다시 시도하고, 남으면 다음 시작 때 R16, R17이 정리한다. 30초 대조는 R42, R67, R83을 하고, R81의 삭제는 runner마다의 타이머가 정확히 180초에 한다.
6. `listener.Run`이 취소가 아닌 오류로 끝나면 R25나 R51을 따른다. 라이브러리는 세션 갱신의 404에서 스스로 복구하지 못한다(확인함: actions/scaleset 이슈 #123). 종료는 drain 규칙을 따르고, 세션은 `context.WithoutCancel`에 타임아웃을 붙여 닫는다. 오류 drain에서 첫 신호로 끝내는 이유(R21): 운영자가 멈추기를 요청한 것이고, 그러지 않으면 오류 대기에 들어간다.

**재시도 규칙**. 영구 오류는 자동으로 다시 시도하지 않고 사람이 고친다(예외 R96). 오류 drain은 세션, scale set, `AcquireJobs` 호출의 영구 오류로 좁히고, 분류는 호출 위치로만 한다.

| 호출 위치 | 일시 오류(전송 오류, 5xx, 속도 제한, 409 세션 충돌, R50의 Docker 삭제 오류) | 영구 오류(그 밖의 4xx) |
|---|---|---|
| 시작 단계(Docker 준비, 이미지 두 개 확인, R16 목록, scale set, `SetSystemInfo`, 첫 세션) | 1~60초 백오프, 성공할 때까지(R24, R52) | 오류 대기(R46~R48), `status`에 표시 |
| 시작 단계 정리(R16 삭제, R17 목록, `GetRunnerByName`, `RemoveRunner`) | 대상 하나는 로그 후 건너뜀, 목록은 3번까지 다시 시도 후 건너뜀(R17) | 로그 후 건너뜀(R17) |
| 메시지 세션 호출(`GetMessage`, `DeleteMessage`, 세션 갱신)과 세션 재생성 | `Run`이 끝나고 1~60초 백오프로 새 세션(R25). 404(세션 없어짐)도 같음 | R51 |
| `Scale()` 안의 `AcquireJobs` | 오류 반환, 새 세션에서 메시지를 다시 받음 | R51 |
| runner 시작(JIT, 네트워크, BuildKit, runner 컨테이너) | 재시도 없음, R61로 셈, 다음 poll에서 수렴(R64) | 다음 poll에서 R61 백오프 아래 다시 시도(R96), 오류 drain 없음 |
| 축소의 `RemoveRunner`(R37) | 다음 `Scale`에서 다시(R93) | 재시도 없음, 대기 runner로 남김(R94) |
| 감시와 대조의 삭제(컨테이너, 네트워크, 볼륨, `RemoveRunner`) | 백오프 최대 10번, 남으면 다음 시작 때 R16, R17 | 재시도 없음, 로그와 마지막 오류, 다음 시작 때 R16, R17 |
| 판정의 workflow run 조회 | 재시도 없음, 그 job만 거부(R65) | 같음 |
| 세션 닫기 | 로그만(R99) | 로그만(R99) |

- 판정을 예외로 두는 이유: 영향이 그 job 하나에 그치고, 다시 시도하면 R81의 180초를 넘길 수 있다. run 조회의 HTTP 타임아웃은 30초다.
- 라이브러리 HTTP 재시도는 전송 오류와 5xx에만 쓴다. v0.5.0 기본 설정은 429를 다시 시도하며 `Retry-After`를 상한 없이 따르고 MaxElapsed가 없다(확인함: `http_client.go` 244~262행, 370~386행). 그래서 `WithRetry`로 `OnRateLimit`이 `(0, false)`를 돌려주게 하고 R52는 스케일러가 지킨다.

**판정**
1. 스케일러는 runner를 만들 때 32바이트 임의 토큰을 runner 표에 두고 stdin으로 넘긴다. pool은 runner 표에서 찾는다. 토큰을 두는 이유: 첫 조회가 busy 시각을 기록하므로(R81), 토큰이 없으면 같은 Docker의 다른 컨테이너(빌드 중인 BuildKit 포함)가 남의 대기 runner 이름으로 조회해 그 runner를 지워지게 하거나 판정을 볼 수 있다.
2. `JobStarted`를 받으면 판정 고루틴이 run을 조회하고 R9의 조건을 확인한다. 저장소 이름은 대소문자를 구분하지 않는다. 값은 모두 GitHub 서버의 메시지와 REST에서 오므로 job 코드가 바꿀 수 없다. `JobStarted`는 최대 약 50초 늦게 올 수 있다(확인함: actions/scaleset 이슈 #122, open).
3. R9의 조건이 막는 경우:
   - reusable 호출: 다른 브랜치나 `main` 태그에서 main의 reusable workflow를 `@main`으로 부르면 `JobWorkflowRef`는 main처럼 보이지만 checkout은 그 커밋을 받는다. 메시지에는 최상위 workflow 필드가 없으므로(확인함: scaleset v0.5.0 `types.go`의 `JobMessageBase`) 경로를 run의 `path`와 같게 요구해 reusable 호출을 모두 거부한다.
   - `main` 태그의 허용 workflow가 같은 파일을 `@main`으로 부르면 경로가 같아질 수 있으므로 `referenced_workflows`를 본다. reusable 호출이 없는 run은 목록 응답과 단일 run 응답 모두에서 이 필드가 `[]`다(확인함: 2026-11-24, 공개 저장소 응답).
   - `main` 태그 자체는 ref가 `refs/tags/main`이라 거부되고, fork의 `main` 브랜치는 `head_repository`로 거른다. `allowed-events`는 `push`, `workflow_dispatch`, `schedule`로 제한해 외부 입력으로 main 코드가 도는 이벤트(`pull_request_target`, `workflow_run`)를 막는다.
4. 거부면 runner 자원 삭제를 먼저 시작한 뒤 거부를 기록하고(R11) 이유를 로그에 남긴다. hook 실패 뒤 `always()` step이 돌 수 있기 때문이다(미확인, Areas of concern 6).
5. 시간 순서: 180초는 `JobStarted` 지연의 세 배 넘는 여유다. hook의 첫 조회가 닿으면 삭제(busy + 180초)가 hook 한도(240초)보다 먼저 오고, 닿지 못해도 허용 없이는 step이 돌지 않는다. 세션이 없는 동안(R25 백오프, R54)에는 허용될 job도 180초 뒤 지워진다. hook이 전혀 닿지 않으면 R83으로 job 시작부터 최대 210초 안에 지워진다. 컨테이너 나이로 지우지 않는 이유는 대기 runner도 판정 없이 오래 살기 때문이다. `Runner.Worker`가 이 이미지의 프로세스 목록에 그 이름으로 보이는지는 미확인이다.
6. `JobWorkflowRef` 형식은 `owner/repo/.github/workflows/x.yml@refs/heads/main`으로 예상한다(미확인). 다르면 거부되므로 허용이 잘못 나가지 않고 R8이 실패해 드러난다.
7. AWS 자격은 job이 `permissions: id-token: write`와 GitHub OIDC로 Role을 직접 받는다. AWS의 main 제한은 관리자 신뢰 정책이 맡고 판정은 그와 별개다. 스케일러 재시작 때는 runner 표가 사라지고 R16이 이전 runner 자원을 지운다.

**외부 계약**
- runner 컨테이너 Env(R70): `SCALER_JOBCHECK_URL`(`http://host.docker.internal:<jobcheck.listen의 포트>`), `SCALER_RUNNER_NAME`, `BUILDKIT_HOST`(`tcp://<runner 이름>-buildkit:1234`). 모두 비밀이 아니다.
- `GET /v1/verdict`: 요청 헤더 `Authorization: Bearer <token>`, `X-Runner-Name: <name>`. 200 응답 JSON `{"verdict":"allow|deny|pending"}`, 401 응답 JSON `{"reason":"..."}`. 같은 요청은 같은 응답을 준다(첫 조회의 busy 기록만 예외).
- status socket: 접속하면 서버가 JSON 한 줄 `{"state":"starting|running|draining|error-wait","pools":[{"name","busy","idle","startFailures","lastStartError","lastStartErrorAt"}],"lastError","lastErrorAt"}`을 쓰고 닫는다. `idle`은 시작 중과 대기 runner 수의 합, `busy`는 job을 실행 중인 runner 수, `lastError`는 R31대로 가린 값이고 설정 오류면 오류 전부다.
- `status` 출력(`마지막 오류`는 없으면 `없음`. pool 줄은 `<이름>: 실행 중 <busy>, 대기 <idle>, 연속 시작 실패 <startFailures>`이고 실패가 있으면 `(<lastStartError>, <lastStartErrorAt>)`를 붙인다): `starting`은 `상태: 시작 중`과 마지막 오류, 0(R58). `running`은 `상태: 정상`, pool 줄, 마지막 오류, 0(R26, R86). `draining`은 `상태: drain 중`, pool 줄, 마지막 오류, 0(R56). `error-wait`는 `상태: 오류 대기`, `오류: <lastError> (<lastErrorAt>)`, 1(R45). 연결 실패는 `실행 중 아님`, 1(R27).
- 설정 파일(TOML, 모르는 키 거부, 비밀 값은 경로로만 참조):

| 키 | 기본값 | 허용 범위 / 설명 |
|---|---|---|
| `github.app-client-id`, `-installation-id`, `-private-key-file` | (필수) | 비어 있지 않은 값, 양의 정수, 권한 0600 이하의 파일. 모든 pool이 같이 쓴다 |
| `runner.image`, `buildkit.image` | (필수) | 로컬 이미지 ID `sha256:…`, `moby/buildkit:<버전>-rootless@sha256:<digest>`(digest 필수). 둘 다 로컬에 있어야 함 |
| `jobcheck.listen` | `127.0.0.1:8765` | loopback 주소만 |
| `shutdown.drain-timeout` | `10m` | 0보다 큼 |
| `[[pools]]` | (필수) | 이번 버전은 정확히 1개(R68) |
| `pools.name` | `linux-arm64-deploy` | 소문자, 숫자, `-`, 1~40자. scale set 이름 = 라벨 = `runs-on`. runner 이름 길이 한도가 미확인이라 여유를 둔 값이다 |
| `pools.repo-url` | (필수) | `https://github.com/<owner>/<repo>`. R9의 대상 저장소 |
| `pools.max`, `pools.idle` | 2, 0 | `max` 1 이상(상한 없음), `idle` 0 이상 `max` 이하 |
| `pools.runner-cpus`, `-memory`, `-pids` | 1, `1g`, 512 | 0보다 큼, 256m 이상, 64 이상. 상한 없음 |
| `pools.build-cpus`, `-memory`, `-pids` | 2, `2g`, 1024 | 0보다 큼, 256m 이상, 64 이상. 상한 없음 |
| `pools.allowed-workflows` | (필수) | 1개 이상, `.github/workflows/<파일>` |
| `pools.allowed-events` | `["push"]` | `push`, `workflow_dispatch`, `schedule` 중 1개 이상 |

- 기본값 합계는 CPU 6, 메모리 6GB이고 Docker VM(10 CPU, 7.75GiB)에 든다(확인함: 2026-11-24 조회). 저장소에는 자리표시자만 넣은 예시 파일을 둔다. pool마다 다른 이미지가 필요하면 pool에 선택 키를 더하고, 없으면 전역 값을 쓴다.
- 운영: 로그는 stderr에 JSON(`log/slog`)으로 쓰고 LaunchAgent가 파일로 남긴다. LaunchAgent(R44)는 `RunAtLoad` true, `KeepAlive`의 `SuccessfulExit` false, `ThrottleInterval` 60, `ExitTimeOut` drain-timeout + 60초다. `SuccessfulExit` false면 launchd는 종료 코드가 0이 아닐 때만 다시 띄우고, `ThrottleInterval`은 다시 띄우는 최소 간격이며, 멈출 때 SIGTERM 뒤 `ExitTimeOut`만큼 기다려 SIGKILL을 보낸다(확인함: `man launchd.plist`). 오류 대기는 프로세스가 살아 있으므로 다시 띄우는 반복이 없다. 멈추기는 `launchctl bootout gui/$(id -u)/<label>`, 고친 뒤 재시작은 `launchctl kickstart -k gui/<uid>/<label>`이다.

**받아들인 trade-off**
- 판정이 `JobStarted`를 기다리므로 첫 step이 최대 180초 늦어질 수 있다. 거부된 job은 잠시 runner 자리를 차지하고, 실패 화면은 runner 끊김으로 보일 수 있다. 빌드 캐시를 runner끼리 나누지 않으므로 빌드마다 처음부터 받는다.
- BuildKit 컨테이너만 `seccomp=unconfined`이고 no-new-privileges를 켤 수 없다. process sandbox가 없으므로 빌드 단계가 buildkitd 메모리에 있는 그 job의 레지스트리 push 자격을 읽을 수 있지만, 영향은 같은 job 안에 그친다.
- actions/scaleset은 v0.5.0으로 고정하고, `WithHTTPClient`로 HTTP/2 ping(read-idle 30초, 응답 대기 10초)을 설정한 transport를 넣는다. 기본 transport는 끊긴 HTTP/2 연결에서 약 25분 멈출 수 있고 수정은 미릴리스다(확인함: actions/scaleset 이슈 #131, 수정 #142는 main에만 있음).

**가정** 관리자 쪽 사실은 관리자 안내문에 적힌 대로 보고, 적히지 않은 것은 플랫폼 기본값으로 가정한다(결정: 2026-11-24, 사용자).
- 안내문은 허용된 네트워크의 PC에 둔 self-hosted runner가 OIDC로 Role을 받는다고 하므로 이 호스트의 컨테이너도 된다고 본다. 2026-11-19~20 시험은 안내문 전의 키 기반 임시 자격이라 네트워크 도달만 증명한다. push는 허용된 네트워크에서만 된다(안내문).
- 레지스트리는 팀 공용 저장소 하나이고 태그를 덮어쓸 수 없다(안내문). 삭제 권한은 기본값대로 없다(IAM 기본 거부). 시험 push는 몇 MB 이미지로 3번 이하로 하고 남은 태그를 보고한다(결정: 2026-11-24, 사용자).
- Role의 `MaxSessionDuration` 기본값은 1시간이고 `configure-aws-credentials`의 `role-duration-seconds` 기본값도 3600초다(확인함: AWS IAM 문서와 그 action README의 기본값, 원문은 다시 읽지 않음). 1시간보다 긴 job은 범위 밖이고 스케일러는 갱신하지 않는다.
- 판정은 main의 코드를 믿는다(대상 저장소의 main 보호 규칙은 미확인). runner 메모리 1GB로 배포 job이 끝난다고 본다(미확인). 모자라면 `runner-memory`만 바꾼다.
- DisableUpdate로 runner 버전이 이미지에 고정된다. GitHub가 오래된 버전에 job을 주지 않게 되는 기한은 미확인이고, 새 버전이 나오면 runner 이미지를 다시 빌드하고 `runner.image`를 바꾼다.

**다음 INTENT로 넘긴 것**: reusable workflow 허용 규칙, `environment:`를 쓰는 job(OIDC subject에 environment 이름이 들어간다는 GitHub OIDC 문서 문구는 확인함, ID가 들어간 subject 형식에서 같은지는 미확인), 기존 workflow의 `runs-on: self-hosted`를 `linux-arm64-deploy`로 바꾸기, 첫 `kubectl apply -f`를 main workflow에서 돌리기(안내문상 액세스 키가 없으므로, 그 이벤트를 `allowed-events`에 넣어야 함), 팀 서비스 빌드의 메모리 측정.

**테스트**
- 단위 테스트는 가짜 scaleset 클라이언트, 가짜 GitHub REST, 가짜 Docker로 한다(`go test ./...`). 실제 GitHub, Docker, 클러스터, 레지스트리를 쓰는 시험은 `integration` 빌드 태그로 나누고 사용자 승인 후에만 돌리며, 시험 자원은 바로 지우고 보고한다.
- 시험 전에 사용자가 시험 저장소에 App을 설치하고, 시험 workflow를 그 main에 쓰는 것은 승인 후다. 시험 절차와 R별 확인 방법은 PLAN에서 정한다.

## How each constraint in the intent is met

| Constraint | Met by |
| :---- | :---- |
| 관리자의 접속 제한을 우회하지 않는다 | R8, R12. 설계: 가정(job은 허용된 네트워크 호스트의 Docker 출구에서 OIDC로 Role을 받음) |
| 클러스터 안 runner는 못 쓴다 | 설계: 구성 요소(호스트 Docker 컨테이너만 사용) |
| Go와 `actions/scaleset` | 설계: 구성 요소(scaleset v0.5.0 고정) |
| 개인 계정 GitHub App, 저장소 단위 등록, 시험 저장소부터 | R22, R68. 설계: 구성 요소(`NewClientWithGitHubApp`, `pools.repo-url`) |
| runner 이름과 `runs-on`은 `linux-arm64-deploy`, 추가 라벨 없음 | 설계: 설정 표(`pools.name`), 스케일링 흐름 1(`Labels` 비움) |
| 기본값(pool마다 최대 2, 대기 0, runner 1 CPU·1GB, 빌드 2 CPU·2GB), 모두 설정 가능, 코드 상한 없음 | R1, R7, R28, R29, R34, R74. 설계: 설정 표 |
| runner 비root, 소켓 없음, 제한, privileged 없음, 호스트 Docker나 다른 runner 제어 경로 없음 | R32~R34, R36, R43, R70, R72, R73, R75, R79 |
| 빌드는 runner마다 스케일러가 띄우고 지우는 rootless BuildKit, job은 요청만 | R62, R63, R67, R71, R73, R76. 설계: BuildKit 컨테이너 |
| `seccomp=unconfined`는 빌드 컨테이너에만, 맞춤 프로필이 되면 교체 | R72. 설계: BuildKit 컨테이너, Areas of concern 8 |
| 스케일러는 자격을 넣지 않고, AWS main 제한은 관리자 신뢰 정책 | R12. 설계: 판정 7 |
| 이미지 빌드와 push를 범위에 넣는다 | R69, R71, R77. 설계: 빌드 경로 |
| CI는 나중에 별도 pool, 섞지 않음 | 설계: pool(설정 형식이 여러 pool을 담음), R68 / (채택 안 함: 여러 pool 동시 운영은 이번에 받지 않음. 오류 범위 미정, Areas of concern 2) |
| 이번에는 시험 저장소에 CD pool 하나, main 쓰기는 승인 후, App 설치는 사용자 | R68. 설계: 테스트 |
| 재부팅 뒤 사람 없이 로그인과 Docker 시작 | R19, R44. 자동 로그인과 Docker AutoStart(확인함: 2026-11-24 조회), 잠자기와 정전 복구는 Areas of concern 9 |
| 공개 저장소, 라이선스·내부 이름·비밀 값 없음, 커밋·푸시·설정 변경·실제 시험은 승인 후 | R31. 설계: 설정 파일(자리표시자 예시), 테스트(`integration` 태그는 승인 후) |
| (Proposed outcome) pool마다 runner 수, CPU와 메모리, 허용 목록을 바꾸고 pool을 나중에 더할 수 있다 | R1, R9, R34, R74. 설계: 설정 표의 pool별 키 / (채택 안 함: pool 추가는 R68이 막음, Areas of concern 2) |

## Open questions from the intent

- 없음. INTENT의 Open questions가 "없음"이다.

## Areas of concern

1. R41의 영구 4xx는 자동으로 다시 시도하지 않으므로 사람이 고쳐야 복구된다(R19 예외). 원인이 일시적인 4xx도 같은 처리를 받는다. 예: 재부팅 직후 시계 차이로 인한 401. 라이브러리는 JWT iat를 60초만 앞당긴다(미확인: 실제 발생 빈도). Owner: 사용자.
2. 여러 pool을 허용할 때 한 pool의 영구 오류(R51)가 다른 pool의 drain과 오류 대기에 어디까지 미칠지 정하지 않았다. 정하기 전에는 R68을 넓히지 않는다. Owner: 사용자.
3. drain 중 `AcquireJobs`로 받았지만 runner를 받지 못한 job이 재시작 뒤 시작되는지 미확인이다(R89). 실제 시험에서 확인한다. Owner: 사용자.
4. `JobStarted`가 180초보다 늦게 오면 허용될 job도 지워진다(미확인: 지연 분포). 넘으면 runner 이름으로 GitHub REST에서 job을 찾는 대안을 검토한다. Owner: 사용자.
5. `main` 태그의 허용 workflow가 같은 파일을 `@main`으로 부르면(main의 파일에 `workflow_call` 필요) R9의 나머지 여덟 조건을 통과할 수 있다. GitHub가 이를 막는지, 그때 `referenced_workflows`가 실제로 채워지는지는 미확인이다. Owner: 사용자.
6. hook이 0이 아닌 값으로 끝날 때 첫 step 전에 job이 끝나는지, `always()` step이 도는지 미확인이다. R11의 삭제 뒤 GitHub가 job을 실패로 표시하기까지의 시간과 다시 큐에 넣는지도 미확인이다. Owner: 사용자.
7. 데몬 없이 `docker login`(`aws-actions/amazon-ecr-login`이 부름), `docker buildx create --driver remote`, `docker buildx build --push`가 되는지, 두 줄 stdin 전달이 되는지 미확인이다. login이 안 되면 대안은 workflow step이 `aws ecr get-login-password`로 받은 값을 `~/.docker/config.json`의 `auths`에 직접 쓰는 것이다(파일은 컨테이너와 함께 지워진다). Owner: 사용자.
8. BuildKit의 `seccomp=unconfined`는 허용된 job의 빌드 단계가 Docker VM 커널의 모든 syscall에 닿게 한다. 맞춤 seccomp 프로필로 좁힐 수 있는지 미확인이고 구현 중 시험한다. Owner: 사용자.
9. 호스트: 자동 로그인과 Docker AutoStart는 켜져 있다(확인함: 2026-11-24 조회). 전원 프로필 하나가 sleep 1이고, 전원 연결 중 잠자기 여부, 정전 뒤 자동 켜짐, 이 Mac이 실제 운영 호스트인지는 미확인이다. Owner: 사용자.
10. 사용자 정의 네트워크의 컨테이너가 `host.docker.internal`로 호스트 listener에 실제로 접속되는지 미확인이다(이름 풀이는 확인함: 2026-11-24). 안 되면 hook이 판정을 받지 못해 모든 job이 실패한다. Owner: 사용자.
11. 고정할 runner 버전에서도 workflow `env:`가 hook에 들어가지 않는지 미확인이다(main 소스는 확인함). 들어가면 `BASH_ENV`나 `LD_PRELOAD`가 "Set up job"에서 내려받은 action 파일을 가리켜 hook 안에서 코드가 돌 수 있다. Owner: 사용자.
12. 컨테이너는 `host.docker.internal`로 호스트의 모든 loopback 서비스에 닿는다(확인함: 2026-11-20). R79는 Docker 2375만 확인한다. 다른 개발 서버나 관리 포트가 이 호스트에서 열려 있는지는 미확인이다. Owner: 사용자.
13. 빌드 디스크 사용량에 상한이 없다. 익명 볼륨은 Docker VM 디스크를 쓰고, 큰 빌드나 R16 이전에 남은 볼륨이 디스크를 채울 수 있다. 라벨 없는 익명 볼륨은 컨테이너 없이 남으면 찾지 못한다. Owner: 사용자.
14. App 토큰(Administration 읽기 쓰기)으로 scale set 삭제(R23)와 저장소 runner 목록(R17)이 되는지 미확인이다. 2026-11-20의 해당 시험 일부는 다른 토큰으로 했다. R17은 시작을 막지 않으므로 목록 권한이 없으면 고아 등록이 `offline`으로 남을 뿐이다. 구현 중 시험한다. Owner: 사용자.
