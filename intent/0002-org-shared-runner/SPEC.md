# Spec: 조직 공용 runner 서버

From: INTENT.md (dewevrei, ac288d0). Status: accepted.
Skills applied: actions-scaleset
Extends: 0001

## Requirements

설정과 target (R105~R107)
- R105. 설정의 `[[targets]]`가 정확히 1개가 아니면(0개나 2개 이상) R28과 같이 오류 대기에 들어가고, 지원하는 개수와 받은 개수를 마지막 오류와 로그에 남긴다(R68을 대체).
- R106. `targets.url`이 `https://github.com/<조직>` 꼴(경로 조각 하나, 끝 `/`와 `.git` 없음)이 아니면 R28과 같이 오류 대기에 들어간다.
- R107. 설정 파일에 `[[pools]]`나 `github.app-installation-id`가 있으면 R28과 같이 오류 대기에 들어가고, 그 키마다 0001 형식의 키라는 것과 대신 쓸 키를 적은 오류를 보인다(Design 설정 파일의 문구).

GitHub 등록 (R108~R113)
- R108. 스케일러는 시작할 때 target마다 App JWT로 `GET /orgs/{org}/installation`을 불러 설치 ID를 얻고, 그 target의 scale set 클라이언트와 REST 클라이언트는 그 설치 ID를 쓴다. 이 호출은 시작 단계 호출로 R24의 재시도와 R46의 오류 대기를 따른다(R24와 R46의 시작 단계 호출 목록에 더한다).
- R109. R108의 응답이 404면 스케일러는 오류 대기에 들어가고, 마지막 오류에 `GitHub App is not installed on organization <org>`를 보인다.
- R110. R108 응답의 `permissions.organization_self_hosted_runners`가 `write`가 아니면 스케일러는 오류 대기에 들어가고, 빠진 권한 이름(`organization_self_hosted_runners: write`)과 조직 이름을 마지막 오류에 보인다.
- R111. `targets.runner-group`이 비어 있으면 scale set을 runner group ID 1에 두고, 값이 있으면 `GetRunnerGroupByName`으로 찾은 group에 둔다. 그 이름의 group이 없으면 오류 대기에 들어간다. 이 조회는 시작 단계 호출로 R24와 R46을 따른다(두 목록에 더한다).
- R112. target마다 R111의 runner group에 이름이 `scaling.label`인 scale set이 없으면 만들고, 있으면 다시 쓴다. 종료해도 지우지 않는다(R22를 대체).
- R113. 스케일러가 만든 runner는 조직 수준에 등록되어, 실행 중에 조직 runner 목록(`GET /orgs/{org}/actions/runners`)에 그 이름으로 나타난다.

동시 실행 한도 (R114~R117)
- R114. 모든 target의 runner 컨테이너 수 합(끝난 runner 포함)은 `scaling.max-runners`를 넘지 않는다(R3을 대체).
- R115. target의 목표 runner 수는 `min(max-runners, 그 target의 idle + 그 target scale set의 TotalAssignedJobs)`이고, 존재하는 runner가 목표보다 적으면 모든 target의 runner 수 합이 `max-runners`에 닿기 전까지 그 차이만큼 시작한다(R1과 R2를 대체).
- R116. 스케일러는 poll마다 `maxCapacity`로 `max-runners`를 보낸다. drain 중에도 같다(R90을 대체).
- R117. 모든 target의 runner 수 합이 `max-runners`보다 적고 두 이미지가 로컬에 있을 때, 스케일러가 job의 첫 메시지(`JobAvailable` 또는 `JobAssigned`)를 받은 시각부터 GitHub job 기록의 `started_at`까지 60초를 넘지 않는다. R87의 경우는 뺀다(R4를 대체).

자원 제한과 Docker 용량 (R118~R122)
- R118. 시작할 때 `runner.cpus`나 `buildkit.cpus`가 Docker가 보고한 CPU 수(Engine API `/info`의 `NCPU`)보다 크면 스케일러는 오류 대기에 들어가고, 그 키, 값, Docker CPU 수를 마지막 오류에 보인다. `/info` 호출은 시작 단계 호출로 R24와 R46을 따른다(두 목록에 더한다).
- R119. `max-runners × (runner.memory + buildkit.memory)`가 Docker가 보고한 메모리(`/info`의 `MemTotal`)보다 크면 시작 로그에 경고를 남기고 계속 실행한다(R29를 대체).
- R120. `max-runners × (runner.cpus + buildkit.cpus)`가 `NCPU`보다 크면 시작 로그에 경고를 남기고 계속 실행한다.
- R121. runner 컨테이너의 CPU, 메모리, 프로세스 수 제한은 `runner.cpus`, `runner.memory`, `runner.pids`와 같고, 메모리 스왑은 없다(R34를 대체).
- R122. BuildKit 컨테이너의 CPU, 메모리, 프로세스 수 제한은 `buildkit.cpus`, `buildkit.memory`, `buildkit.pids`와 같고, 메모리 스왑은 없다(R74를 대체).

판정: fork pull request 거부 (R123~R133)
- R123. 스케일러는 job 시작 hook이 보낸 이벤트 보고(이벤트 이름과 이벤트 파일 내용)만으로 판정을 내리고, 판정하려고 `JobStarted`를 기다리거나 GitHub에 요청을 보내지 않는다(R9를 대체하고 R65를 없앤다).
- R124. 끝까지 읽은 이벤트 보고의 본문이 JSON 객체 정확히 하나가 아니면(빈 본문, JSON이 아닌 본문, 배열 같은 객체 아닌 값, 객체 뒤에 공백 말고 다른 값이 더 있는 본문) 거부한다. 수는 정밀도를 잃지 않게 원래 표기대로 읽는다.
- R125. 이벤트 이름이 `^[a-z_]{1,64}$`에 맞지 않으면(빈 이름 포함) 거부한다.
- R126. 본문의 최상위 `pull_request`가 있고 null이 아니면, `pull_request.head.repo.id`, `pull_request.base.repo.id`, 최상위 `repository.id`가 모두 양의 정수(소수점과 지수가 없는 1 이상 2^63−1 이하의 수)이고 셋이 같을 때만 이 규칙을 통과한다. 그 밖에는(값이 없거나 null이거나 정수가 아니거나 서로 다르면) 거부한다. 이벤트 이름과 관계없이 적용한다.
- R127. 본문의 최상위 `workflow_run`이 있고 null이 아니면, `workflow_run.head_repository.id`, `workflow_run.repository.id`, 최상위 `repository.id`가 모두 R126의 양의 정수이고 셋이 같을 때만 이 규칙을 통과한다. 그 밖에는 거부한다. 이벤트 이름과 관계없이 적용한다.
- R128. 이벤트 이름이 `pull_request`, `pull_request_target`, `pull_request_review`, `pull_request_review_comment` 중 하나인데 최상위 `pull_request`가 없거나 null이면 거부하고, 이벤트 이름이 `workflow_run`인데 최상위 `workflow_run`이 없거나 null이면 거부한다.
- R129. R124~R128 중 어느 것으로도 거부되지 않은 보고는 허용한다. 판정은 `head.repo.fork`, 저장소의 `full_name`, `author_association`, 브랜치, ref, workflow 경로를 쓰지 않는다.
- R130. 거부할 때 스케일러는 0001의 로그 줄 `job denied; waiting for the runner to stop`에 runner 이름, 이벤트 이름, `reason`(Design 판정의 거부 사유 값)을 남기고, 본문에 `pull_request.head.repo.full_name`이나 `workflow_run.head_repository.full_name`이 문자열로 있으면 그 값을 `head_repo`로 더한다.
- R131. 토큰이 맞는 그 runner의 첫 이벤트 보고를 받으면, 스케일러는 본문을 다 읽은 때부터 1초 안에 판정을 기록하고 응답에 담는다. 예외로 drain 끝의 정리가 시작된 뒤 받은 보고는 판정하지 않고 `pending`을 돌려준다.
- R132. 판정이 기록된 runner의 뒤 이벤트 보고는 본문을 읽지 않고 기록된 판정을 돌려주며, 판정을 바꾸지 않는다.
- R133. 세션을 쓸 수 없는 동안(R25의 백오프, R54, R92)에도 스케일러는 이벤트 보고를 받으면 판정한다(R54와 R92의 "`JobStarted`를 받지 못해 R81로 지워진다"를 대체).

job 확인 서버와 hook (R134~R140)
- R134. job 확인 서버는 `POST /v1/event`의 본문을 읽기 전에 토큰을 검증하고, 실패하면 본문을 읽지 않고 바로 401을 돌려주며 판정과 그 runner의 busy 시각(R81)을 기록하지 않는다(R40을 `POST /v1/event`로 넓힌다).
- R135. `/v1/event`에 `POST`가 아닌 요청이 오면 job 확인 서버는 `Allow: POST` 헤더와 함께 405를 돌려주고, 판정과 busy 시각을 기록하지 않는다.
- R136. 토큰이 맞는 첫 이벤트 보고의 본문이 32MiB(33,554,432바이트)를 넘으면, job 확인 서버는 32MiB + 1바이트에서 읽기를 멈추고 거부(`reason` `payload-too-large`)를 기록한 뒤 413과 `{"verdict":"deny"}`를 돌려준다.
- R137. 32MiB 안에서 본문 읽기가 오류로 끝나면(연결이 닫힘, `Content-Length`보다 적게 옴, 읽기 시한), job 확인 서버는 판정하지 않고 아무것도 기록하지 않은 채 그 연결을 닫는다. 그 runner의 다음 보고는 R131의 첫 보고로 처리한다.
- R138. 토큰이 맞는 첫 `POST /v1/event`를 받은 때도 R81의 busy가 된 때의 후보다(R81의 busy 계기에 더한다).
- R139. job 시작 hook은 판정을 조회하기 전에, 자기 시작 때 환경의 `GITHUB_EVENT_NAME` 값과 `GITHUB_EVENT_PATH`가 가리키는 파일 내용을 `POST /v1/event`로 보낸다. 파일이 없거나 읽을 수 없으면 빈 본문을 보내고, `GITHUB_EVENT_NAME` 값이 `^[a-z_]{1,64}$`에 맞지 않으면 빈 이벤트 이름을 보낸다(R66의 "판정을 조회만 하고"를 대체).
- R140. hook은 `POST /v1/event`의 응답이 200이고 `verdict`가 `allow`나 `deny`면 R66대로 처리하고, 200의 `pending`, 200과 401 밖의 상태 코드(400, 405, 413, 5xx 등), 해석할 수 없는 응답이면 다시 보내지 않고 R66대로 `GET /v1/verdict`를 2초마다 조회하며, 연결 실패나 401이면 2초 쉬고 `POST /v1/event`를 다시 보낸다. 어느 경우에도 허용을 받지 않으면 0으로 끝나지 않는다.

실제 실행 확인: 판정 (R141~R147)
- R141. 조직 저장소의 main이 아닌 브랜치에서 시작한 job은 허용되어 모든 step이 돈다.
- R142. R141의 job에서 관리자 안내문의 Role을 OIDC로 받는 step은 실패한다(관리자 신뢰 정책의 결과를 실제 실행으로 확인한다).
- R143. 같은 저장소 main의 job은 조직 수준에 등록된 runner에서 GitHub OIDC로 관리자 안내문의 Role을 받아 안내문의 Namespace에서 Pod 목록 조회에 성공한다(0001 R8과 같은 확인. R142의 실패가 OIDC 경로의 고장이 아니라 신뢰 정책의 거부임을 가른다).
- R144. 같은 저장소의 브랜치에서 연 pull request의 `pull_request` 이벤트로 시작된 job은 허용되어 모든 step이 돈다.
- R145. fork에서 연 pull request의 `pull_request` 이벤트로 시작된 job은 "Set up job" 뒤 step이 `if: always()` step을 포함해 하나도 돌지 않고 실패로 끝나며, 판정부터 40초 안에 runner 컨테이너가 없어진다.
- R146. fork에서 연 pull request의 `pull_request_target` 이벤트로 시작된 job은 R145와 같이 끝난다.
- R147. fork에서 연 pull request의 run이 끝나 일으킨 `workflow_run` 이벤트의 job은 R145와 같이 끝난다.

정리와 명령 (R148~R152)
- R148. 스케일러는 시작할 때 target마다 조직 runner 목록(`GET /orgs/{org}/actions/runners`, 모든 페이지)에서, 이름이 `<scaling.label>-` 뒤에 소문자 16진수 정확히 8자가 붙은 꼴이고 `GetRunnerByName`의 `RunnerScaleSetID`가 그 target의 scale set ID와 같으며 살아 있는 runner 컨테이너가 없는 등록을 제거한다. 일시 오류와 영구 오류의 처리는 R17과 같다(R17을 대체).
- R149. R42의 대조는 끝난 runner는 빼고, 허용 판정이 없는 runner마다 조직 runner 목록을 그 이름으로 조회해(`GET /orgs/{org}/actions/runners?name=<runner 이름>`) 결과 중 이름이 정확히 같은 runner의 `busy`가 true면 그 runner를 busy로 표시한다. 조회가 실패하면 로그를 남기고 그 대조에서 그 runner를 건너뛴다(R103을 대체).
- R150. `delete-scaleset [-config <경로>] <target URL>` 명령은 설치 조회(R108)와 runner group 조회(R111)로 찾은 그 target의 scale set과 그 runner 등록을 지우고 0으로 끝난다(R23을 대체).
- R151. R150의 URL이 설정에 없거나, 설치 조회나 runner group 조회가 실패하면(R109, R110의 경우 포함) `delete-scaleset`은 아무것도 지우지 않고 오류를 stderr에 쓴 뒤 1로 끝난다.
- R152. `status` 명령은 `상태: 정상`과 `상태: drain 중`일 때 target 줄과 함께 `전체: 실행 중 <n>, 대기 <m>, 최대 <max-runners>` 줄을 출력한다.

서비스 응답 확인과 로그 (R153~R154)
- R153. 마지막으로 서버가 답한 poll부터 5분이 지난 뒤의 끊지 않는 poll에는 70초 시한을 두고, 그 시한으로 끝나면 스케일러는 그 poll을 일시 오류로 보고 마지막 오류에 남긴 뒤 R25대로 새 세션을 만든다.
- R154. `GetMessage`가 속도 제한 오류로 끝날 때마다 스케일러는 WARN 로그 `message poll rate limited` 한 줄을 상태 코드와 다시 부를 수 있는 시각과 함께 남긴다.

Linux 확장 준비 (R155)
- R155. `GOOS=linux`에서 `GOARCH=arm64`와 `GOARCH=amd64` 모두 `go build ./...`와 `go vet ./...`가 성공한다.

실제 실행 확인 (R156~R165)
- R156. 스케일러가 시작한 뒤 조직의 runner group에 더한 저장소의 job은 스케일러 설정을 바꾸거나 다시 시작하지 않고 `runs-on: linux-arm64-deploy`만으로 runner를 받아 성공한다.
- R157. `max-runners`가 5일 때 서로 다른 저장소 5곳에서 시험 job(`sleep 120` step 하나와 push 없는 `docker buildx build` step 하나, Design 테스트)을 하나씩 10초 안에 시작하면, runner 컨테이너 5개가 같은 때 돌고 5개 job이 모두 성공한다.
- R158. R157의 5개 job이 도는 동안 시작한 여섯째 job은 GitHub 큐에서 기다리고, 다섯 runner 중 하나의 컨테이너가 없어진 때부터 60초 안에 시작한다.
- R159. `service stop` 뒤 `launchctl print gui/<uid>/<label>`은 0으로 끝나고(LaunchAgent가 로드된 채), `launchctl kickstart gui/<uid>/<label>` 뒤 60초 안에 `status`가 `상태: 정상`을 출력하며, 그 뒤 시작한 job이 성공한다.
- R160. job이 도는 중 실행한 `service install`은 그 job이 성공으로 끝난 뒤 0으로 끝나고, 명령이 끝난 때부터 60초 안에 LaunchAgent가 로드돼 있고 `status`가 `상태: 정상`을 출력한다.
- R161. job이 도는 중 실행한 `service uninstall`이 0으로 끝난 뒤 `managed-by` 라벨이 붙은 컨테이너는 0개이고, `launchctl print gui/<uid>/<label>`은 113으로 끝난다.
- R162. Actions 서비스가 poll에 답하지 않게 된 뒤 마지막으로 답한 poll부터 6분 30초 안에 `status`의 마지막 오류에 poll 오류가 나타나고, 서비스가 다시 답한 뒤 시작한 job이 성공한다.
- R163. 실제 실행 시험 동안 시작한 job마다 스케일러 로그에 그 job ID의 `JobStarted`와 `JobCompleted`가 있고, 그중 GitHub job 기록의 `started_at`이나 `completed_at`이 10초로 끊긴 poll 동안이었던 메시지는 그 뒤 poll의 로그에 나타난다(관찰 방법은 Design 테스트).
- R164. 실제 실행 시험 동안 허용될 job이 R81로 지워진 경우는 0건이다.
- R165. 실제 실행 시험(R141~R164)이 모두 끝나면 그 기간의 스케일러 로그에서 R154 로그 줄의 수를 센 값이 기록된다. 따로 연속 실행 시험은 하지 않는다.

## Design

용어: target은 runner를 등록하는 GitHub 대상 하나다. 이번 버전의 target은 조직이다. 0001 SPEC 요구사항의 "pool"은 이 변경에서 target 하나와 그 scale set으로 읽는다. pool마다의 설정값 중 `max`는 서버 전체의 `scaling.max-runners`(R114~R116), `idle`은 `targets.idle`, 자원 제한은 `[runner]`와 `[buildkit]`의 키(R121, R122), pool 이름은 `scaling.label`이다. 그 밖의 0001 요구사항(격리, drain, 재시도, 오류 대기, hook의 kill 규칙, 서비스)은 그대로 성립한다.

**바꾸거나 없애는 0001 요구사항**
- 대체: R1·R2의 목표 계산(R115), R3(R114), R4(R117), R9(R123~R129), R17(R148), R22(R112), R23(R150, R151), R29(R119), R34(R121), R54와 R92의 한 문장(R133), R66의 한 구절(R139, R140), R68(R105), R74(R122), R90(R116), R103(R149).
- 넓힘: R24와 R46의 시작 단계 호출 목록에 설치 조회(R108), runner group 조회(R111), Docker `/info`(R118)를 더한다. R40의 401 규칙을 `POST /v1/event`에도 적용하고 본문을 읽기 전에 검증한다(R134). R81의 busy 계기에 토큰이 맞는 첫 `POST /v1/event`(R138)를 더하고, R81 계기의 "R103의 발견"은 R149의 발견으로 읽는다.
- 그대로: R82는 성립한다. hook은 판정 조회에 여전히 스케일러가 준 주소와 runner 이름만 쓴다. 새로 읽는 `GITHUB_EVENT_NAME`과 `GITHUB_EVENT_PATH`는 runner가 hook 환경에 넣는 값이고, hook 전에 job 코드가 돌지 않으며 workflow `env:`는 hook 환경에 들어가지 않으므로 job 코드가 hook 전에 바꿀 수 없다(설계: 판정의 "이벤트 파일을 믿는 근거").
- 없앰: R65(판정에 GitHub 조회가 없음). R52의 "판정 중의 workflow run 조회와 최신 run 조회는 예외로 R65를 따른다"는 대상 호출이 없어져 적용할 곳이 없다. R98 목록의 workflow run 조회와 최신 run 조회는 없애고, "저장소 runner 목록"은 조직 runner 목록(R148)과 이름 조회(R149)로 바꾼다. 두 호출의 영구 오류도 R98대로 오류 drain을 시작하지 않는다.
- 0001 Design에서 바뀌는 항목: 판정 1~8 중 토큰(판정 1), 거부 처리(판정 4), `JobWorkflowRef` 형식(판정 6), AWS(판정 7)는 그대로 두고, run 조회(판정 2), R9 조건과 `allowed-events` 설명(판정 3), 시간 순서 중 `JobStarted` 의존 부분(판정 5), 최신 run 조건(판정 8)은 없앤다. 판정 3과 판정 8이 막던 경우가 열리는 것은 Areas of concern 4다. 스케일링 흐름 2의 "끊지 않는 poll은 부모 ctx와 라이브러리 시한만으로 보낸다"는 R153의 70초 시한으로 바꾼다. 재시도 표의 "판정의 workflow run 조회와 최신 run 조회" 행은 없애고 "시작 단계" 행에 설치 조회, runner group 조회, `/info`를 더한다. "대조의 runner 목록(R103)" 행은 조직 runner 이름 조회(R149)가 같은 규칙으로 잇는다.

**구성 요소**
- target(`internal/github`): 종류(이번에는 조직만)와 이름을 가진 값이다. 종류마다 다른 것은 네 가지뿐이고 모두 target의 메서드 뒤에 둔다: 등록 URL(`GitHubConfigURL`, 조직은 `https://github.com/<org>`), 설치 조회 경로(`/orgs/{org}/installation`), runner 목록 경로(`/orgs/{org}/actions/runners`), 설치에 있어야 하는 권한(`organization_self_hosted_runners: write`). scaleset v0.5.0은 경로 조각이 하나인 URL을 조직 범위로 읽고 등록 토큰을 `/orgs/{org}/actions/runners/registration-token`에서 받는다(확인함: v0.5.0 `config.go` 55행, `client.go` 1043~1046행). 그래서 scale set 생성, 세션, JIT 발급, `GetRunnerByName`, `RemoveRunner`는 0001과 같은 라이브러리 호출을 그대로 쓴다.
- 설치 ID 조회(R108~R110): 0001은 전역 `github.app-installation-id` 하나를 썼다. 조직과 나중의 개인 계정 저장소는 설치가 서로 다르므로 target마다 App JWT로 조회한다. 이 endpoint는 JWT로만 부르고 응답은 설치 객체(필수 `id`, 필수 `permissions`)다(확인함: https://docs.github.com/en/rest/apps/apps "Get an organization installation for the authenticated app" `GET /orgs/{org}/installation`, "You must use a JWT to access this endpoint.", 응답은 "Get an installation for the authenticated app"과 같은 스키마이고 그 스키마에 "`id`: required, integer", "`permissions`: required"). 그 절의 응답 코드는 200만 적혀 있고, 404는 저장소 설치 조회 절에만 있다. App이 설치되지 않은 조직에서 404가 오는지(R109)는 미확인이다(Areas of concern 5). JWT만 쓰므로 새 App 권한이 필요 없다. 권한 검사(R110)는 첫 등록 호출의 403보다 먼저, 무엇이 빠졌는지 이름으로 알리기 위해 둔다. 조회한 ID는 `ScaleSetConfig.InstallationID`와 `RESTConfig.InstallationID`에 넣고, 서명기(`AppSigner`)는 0001처럼 전역 하나를 같이 쓴다.
- App 권한: 조직 수준 runner에는 Organization permission "Self-hosted runners: Read and write"가 필요하고, Repository permission "Administration: Read and write"는 저장소 범위 등록에만 필요하다(확인함: https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api, "**Administration:** Read and write" 주석 "is only required when configuring Actions Runner Controller to register at the repository scope.", Organization permissions "**Self-hosted runners:** Read and write"). App이 조직의 self-hosted runner endpoint를 부르려면 `organization_self_hosted_runners` 권한이 필요하다(확인함: https://docs.github.com/en/rest/actions/self-hosted-runner-groups 도입부). 판정이 GitHub를 부르지 않으므로(R123) 0001의 Actions 읽기 권한은 이 변경의 호출에 쓰이지 않는다. 이 권한 하나로 우리 호출이 모두 되는지는 Areas of concern 5, 쓰지 않게 된 권한을 뺄지는 Areas of concern 6이다.
- runner group(R111): ARC는 group을 지정하지 않으면 ID 1을 쓰고, 지정하면 `GetRunnerGroupByName`으로 ID를 찾는다(확인함: ARC eca1a62 `autoscalingrunnerset_controller.go` 1025~1034행). 스케일러도 같게 한다. v0.5.0의 `GetRunnerGroupByName`은 그 이름의 group이 없으면 200 응답에서 "no runner group found with name" 오류를 만든다(확인함: v0.5.0 `client.go` 545행). 같은 이름의 scale set 여럿(R41)처럼 상태 코드가 없는 이 오류를 영구 오류로 분류하는 규칙을 `internal/github`의 `convertError`에 더한다. 조직의 기본 group 하나는 모든 조직에 있다(확인함: https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access "All organizations have a single default runner group."). 그 ID가 1인지는 ARC의 관례이고 문서에는 없다(미확인, Areas of concern 7). 어느 저장소가 쓸지는 그 group의 저장소 접근 설정이 정하고, 공개 저장소는 group에서 따로 허용해야 한다(확인함: 같은 문서 "By default, only private repositories can access runners in a runner group, but you can override this").
- `targets.runner-group`을 바꾸면 R112는 새 group에서 같은 이름의 scale set을 찾지 못해 새로 만들고, 옛 group의 scale set은 listener 없이 남는다. 서버가 다른 group에 같은 이름의 scale set을 만들게 두는지는 미확인이다(Areas of concern 7). 만들게 두면 같은 라벨의 scale set이 둘이 되어 job이 listener 없는 쪽에 배정될 수 있으므로(추론), group을 바꾸기 전에 옛 설정으로 `delete-scaleset <target URL>`을 실행해 옛 scale set을 지운다(README에 적는다).
- 공유 자원: runner 표, 동시 실행 한도, Docker 클라이언트, job 확인 서버, status, 서명기는 서버에 하나씩이고 모든 target이 같이 쓴다. target마다 따로인 것은 설치 ID, scale set 클라이언트, REST 클라이언트, scale set, 세션, listener다. runner 표의 행은 target을 기록한다.
- 동시 실행 한도(R114, R115): 한도는 CPU·메모리 합이 아니라 runner 수로 정한다. 이유는 셋이다. (1) GitHub가 보내는 스케일 신호(`TotalAssignedJobs`)와 받는 쪽 한도(`maxCapacity`)가 모두 개수다(actions-scaleset 스킬 references/runtime.md 3절). (2) Docker의 CPU·메모리 제한은 상한이지 예약이 아니어서, 상한 합으로 막으면 실제 사용과 관계없이 동시 실행이 줄어든다. (3) 참고한 runscaler도 호스트 전체 한도를 개수(`concurrent`)로 둔다(확인함: ysya_runscaler 6f33444 `config.example.toml` 8행 "host-wide limit"). 스케일러는 호스트 크기를 가정하지 않는다. 한도와 job마다의 CPU·메모리는 설정이고, 빌드가 더 필요하면 기본값을 올린다. 상한 합이 Docker가 보고한 값을 넘는 설정은 시작 경고(R119, R120)로 드러낸다. 계산은 runner 표의 잠금 아래에서 한다. target의 `Scale`은 시작할 수를 `min(목표 − 존재, max-runners − 모든 target의 runner 수)`로 정한다. 그래서 target이 여럿이 되어도 먼저 `Scale`을 부른 target이 먼저 자리를 얻는다(공정성은 INTENT 범위 밖). 축소(R37)는 target마다 0001과 같다.
- `maxCapacity`(R116): target마다 `max-runners`를 보낸다. 0이면 배정이 보이지 않으므로 낮추지 않는다(0001 R90의 이유, actions/scaleset 이슈 #133). target이 하나인 이번 버전에서는 0001과 같은 효과다. 여럿이면 각 scale set이 `max-runners`까지 배정받아 합이 한도를 넘을 수 있다(Areas of concern 12).
- Docker 용량 확인(R118~R120): Docker 준비와 R16 뒤, 이미지 확인 다음에 `/info`를 한 번 부른다. CPU 상한이 `NCPU`를 넘으면 Docker가 컨테이너 생성을 거부하므로(확인함: 0001 Areas of concern 21, moby `docker-v29.6.1` `daemon/daemon_unix.go` 482~485행) 모든 시작이 실패할 설정을 시작 때 오류 대기로 드러낸다. 메모리와 CPU 합은 상한이라 경고만 남긴다. 경고 문구는 `configured limits exceed what Docker reports`와 키, 합계, Docker 값이다.

**판정(R123~R140)**
- 정책: `internal/gate`의 정책은 `Decide(eventName string, payload []byte) Decision` 하나가 된다. `Decision`은 허용 또는 거부와 거부 사유(`reason`)다. `JobStarted`의 `OwnerName`, `RepositoryName`, `WorkflowRunID`는 판정에 쓰지 않고 로그에만 남긴다. `Policy`의 허용 목록, `latest.go`, REST의 `GetRun`과 `ListWorkflowRuns`는 없앤다.
- 규칙 순서와 거부 사유: 본문 크기(R136, `payload-too-large`) → 본문이 JSON 객체 하나인지(R124, `payload-not-one-object`. `json.Decoder`의 `UseNumber`로 읽고, 첫 값 뒤에 토큰이 더 있으면 거부) → 이벤트 이름(R125, `event-name-invalid`) → 최상위 `pull_request`(R126, 값이 없거나 양의 정수가 아니면 `pull-request-repo-id-invalid`, 셋이 다르면 `pull-request-from-other-repo`) → 최상위 `workflow_run`(R127, 같은 꼴로 `workflow-run-repo-id-invalid`, `workflow-run-from-other-repo`) → 이름과 구조의 대조(R128, `pull-request-missing`, `workflow-run-missing`) → 허용(R129). 한 본문에 `pull_request`와 `workflow_run`이 둘 다 있으면 둘 다 통과해야 한다.
- 이벤트 파일로 판정하는 이유: `pull_request_target`과 `workflow_run`의 run은 기본 브랜치의 마지막 커밋과 기본 브랜치 ref로 돈다(확인함: https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows 두 절의 표 `GITHUB_SHA` "Last commit on default branch", `GITHUB_REF` "Default branch"). 그래서 workflow run REST 응답의 `head_repository`로는 그 run을 일으킨 pull request가 fork에서 왔는지 알 수 없다고 본다(추론). 이벤트 파일은 그 job을 일으킨 webhook payload 전체다(확인함: https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts "The full webhook event payload can be found in `GITHUB_EVENT_PATH`."). fork head 정보가 이벤트에 있다는 것은 GitHub 문서가 `github.event.pull_request.head.repo.full_name`으로 fork를 가르는 예를 들고(확인함: securely-using-pull_request_target, set-up-code-coverage 문서), actions/checkout이 `pull_request_target`에서는 `pull_request.head.repo.id`를 최상위 `repository.id`와, `workflow_run`에서는 `workflow_run.head_repository.id`를 비교하는 데서 본다(확인함: actions/checkout main f548e57 `src/unsafe-pr-checkout-helper.ts` 18~55행). runner-up도 job 시작 hook에서 이 파일만 읽어 구조로 판정한다: 최상위 `pull_request`가 null이 아니면 head와 base 저장소 ID를, 최상위 `workflow_run`이 있으면 `head_repository.id`와 `repository.id`를 같은 양의 정수인지 비교하고, 그 밖은 허용하며, 읽을 수 없거나 빈 파일은 거부한다(확인함: runner-up 013cc74 `internal/agent/forkguard.go`).
- 필드의 근거: OpenAPI의 webhook 스키마에서 pull request의 opened, synchronize, closed는 `head.repo.id`와 `base.repo.id`가 필수이고, review submitted와 review comment created는 `head.repo`가 nullable이며, workflow run은 `workflow_run.head_repository.id`와 `workflow_run.repository.id`가 필수다(확인함: github/rest-api-description c9c95fe). octokit/webhooks 스키마는 지운 fork에서 `head.repo`를 null로 허용한다(확인함: octokit/webhooks 스키마). Actions 이벤트 파일에도 null이 오는지는 미확인이지만 null은 R126으로 거부되므로 안전한 쪽이다(Areas of concern 2).
- 구조로 판정하는 이유(R126, R127): 이벤트 이름만으로 PR 계열을 정하면 `pull_request`를 담는 다른 이름의 이벤트를 놓친다. 그래서 이름과 관계없이 최상위 `pull_request`와 `workflow_run`이 있으면 비교한다. 최상위 `repository.id`와도 같아야 하는 것은 head와 base가 서로 같아도 이 run의 저장소가 아닌 경우를 거부하기 위해서다. 이름 규칙(R128)은 이름이 PR 계열이나 `workflow_run`인데 R126, R127이 비교할 객체가 없는 보고를 거부한다. 그런 보고는 R126, R127을 거치지 않으므로, R128이 없으면 R129로 허용된다. 문서의 "The `pull_request` webhook event payload is empty for merged pull requests and pull requests that come from forked repositories."는 REST `GET /repos/{owner}/{repo}/actions/runs`의 `pull_requests` 배열에 대한 문장에서 왔고(확인함: github/docs#22501) 이벤트 파일에 대한 것이 아니다. 그래서 R128은 같은 저장소의 병합된 PR을 거부하지 않는다(OpenAPI상 closed 이벤트에도 `pull_request`가 필수, 아래 필드의 근거).
- 쓰지 않는 값(R129): `head.repo.fork`는 우리 저장소 자체가 다른 저장소의 fork이면 같은 저장소 PR도 true라 잘못 거부한다. `full_name`은 저장소 이름을 바꾸거나 옮기면 달라진다. `author_association`은 관계를 나타낼 뿐 쓰기 권한을 정하지 않는다(확인함: OpenAPI `author-association` enum, GraphQL `CommentAuthorAssociation`의 값 설명). 이벤트 이름만으로는 R128 밖에서 거부하지 않는다.
- 이벤트 파일을 믿는 근거(확인함: actions/runner v2.338.2, 커밋 d1eb12d):
  - runner는 hook handler를 만들고 돌리기(`JobHookProvider.cs` 66~87행) 바로 전에 `executionContext.WriteWebhookPayload()`를 부른다(49~54행). 그 함수는 `GetGitHubContext("event")`를 `_temp/_github_workflow/event.json`에 쓰고 `event_path`를 정하며, event가 null이면 파일도 `event_path`도 만들지 않는다(`ExecutionContext.cs` 1320~1335행, 1328행). 그 값은 서버 job 메시지의 `ContextData["github"]`에서 오고(1001~1005, 1035~1040행), 정수는 Int32나 Int64로, null은 null로 직렬화된다(`NumberContextData.cs` 42~51행).
  - hook은 job의 첫 pre-job step이다(`JobExtension.cs` 74, 309행). container action 빌드(315행), job 컨테이너(328행), action의 pre step(362행)은 그 뒤이고, 그 앞의 `PrepareActionsAsync`(301행)는 action을 내려받기만 한다. 그래서 hook 전에 job이 준 코드는 그 컨테이너에서 돌지 않는다.
  - hook 환경에는 `GITHUB_EVENT_NAME`과 `GITHUB_EVENT_PATH`가 있고(`GitHubContext.cs` 22~23, 52~60행, `ScriptHandler.cs` 290~299행), workflow와 job의 `env:`는 hook 프로세스 환경에 들어가지 않는다(`StepsRunner.cs` 106~112행, hook handler는 빈 환경 사전으로 만든다, `JobHookProvider.cs` 74행). 우리 이미지에서 두 변수가 실제로 있는지는 R145~R147의 기록 시험에서 본다(Areas of concern 1).
  - runner는 action step마다 그 전에 이 파일을 다시 쓴다(`ActionRunner.cs` 141행). 그래서 hook은 경로가 아니라 내용을 보내고, 판정은 hook 안에서 끝난다.
  - run-scripts 문서는 hook이 0이 아닌 코드로 끝나면 job이 돌지 않는다고 적는다(확인함: "If there is any other exit code, the job will not run and will be marked as failed."). 그러나 `Runner.Worker`가 살아 있는 채로 hook이 실패하면 `always()`와 `failure()` step은 돈다(확인함: 0001 Areas of concern 6, `image/job-started.sh` 머리 주석). 그래서 거부는 hook의 종료 코드에 기대지 않고 Worker를 kill한다(0001 R66).
  - 다른 컨테이너는 그 runner의 토큰이 없어 보고할 수 없다(0001 판정 1).
  - 남은 미확인(우리 이미지와 실제 실행의 값, runner를 올릴 때의 순서)은 Areas of concern 1, 2다.
- 판정이 `JobStarted`를 기다리지 않으므로 0001의 `JobStarted` 지연(이슈 #122, 10초 poll로 약 4~5초)이 판정 시간에서 빠진다. R81의 180초 타이머, R83, R149은 hook이 보고하지 못한 runner를 지우는 장치로 그대로 둔다. R102의 job ID 비교도 그대로다.
- 고르지 않은 방법: (1) 0001처럼 `JobStarted`의 `WorkflowRunID`로 run을 REST 조회해 `head_repository.id`와 `repository.id`를 비교(필드는 확인함: https://docs.github.com/en/rest/actions/workflow-runs "`head_repository`: required", "`repository`: required"). `pull_request`는 잡지만 `pull_request_target`과 fork가 일으킨 `workflow_run`은 위 이유로 가를 수 없고, job마다 다른 저장소에 조회가 나가 Actions 읽기 권한, 속도 제한, 조회 실패 거부(0001 R65)가 다시 생긴다. (2) `pull_request_target`과 `workflow_run`을 출처와 관계없이 모두 거부. 같은 저장소에서 일으킨 run까지 막아 "fork pull request만 거부한다"는 사용자 결정을 넘는다(이 결정으로 열리는 경우는 Areas of concern 4). (3) 일으킨 사람의 쓰기 권한을 판정. `author_association`으로는 정할 수 없고 REST 협업자 권한 조회가 필요해 R123과 맞지 않는다(Areas of concern 3).
- job 확인 서버 계약: 0001의 `GET /v1/verdict`에 `POST /v1/event`를 더한다. 요청 헤더는 `Authorization: Bearer <token>`, `X-Runner-Name: <name>`, `X-Event-Name: <이벤트 이름>`이고 본문은 이벤트 파일의 바이트다. 처리 순서와 응답:
  1. `POST`가 아니면 405, `Allow: POST`(R135).
  2. 토큰 검증. 실패하면 본문을 읽지 않고 401과 `{"reason":"..."}`(R134). `host.docker.internal`로는 다른 컨테이너도 이 서버에 닿으므로, 토큰 없는 요청이 32MiB 본문을 거듭 보내도 서버가 읽지 않게 한다. 성공하면 busy 시각을 기록한다(R138).
  3. 판정이 이미 기록됐으면 본문을 읽지 않고 200과 그 판정(R132).
  4. drain 끝의 정리가 시작됐으면 본문을 읽지 않고 200과 `{"verdict":"pending"}`(R131의 예외).
  5. 본문을 32MiB + 1바이트까지 읽는다. 넘으면 거부를 기록하고 413과 `{"verdict":"deny"}`(R136). 읽기가 오류로 끝나면(연결이 닫힘, `Content-Length`보다 적게 옴, 읽기 시한) 판정도 기록도 하지 않고 연결을 닫는다(R137). hook의 curl이 5초 시한으로 끊으면 서버는 잘린 본문을 받으므로, 이를 R124의 거부로 기록하면 다시 보낸 온전한 보고가 R132로 그 거부를 받게 된다. R124는 끝까지 읽은 본문에만 적용한다. 32MiB는 GitHub가 밝힌 webhook payload 상한보다 크게 잡은 값이다(확인함: https://docs.github.com/en/webhooks/webhook-events-and-payloads "Payloads are capped at 25 MB.").
  6. `Decide`로 판정해 기록하고 200과 `{"verdict":"allow"}` 또는 `{"verdict":"deny"}`(R131). 거부면 R130의 로그 줄을 남긴다.
  - 이 서버는 400을 쓰지 않는다. 형식이 틀린 본문은 거부 판정이다(R124, R125).
- hook 변경(R139, R140): hook은 시작할 때 자기 환경에서 `GITHUB_EVENT_NAME`과 `GITHUB_EVENT_PATH`를 읽어 둔다. 보고는 `/usr/bin/env -i /usr/bin/curl -q --noproxy '*' -s -X POST --data-binary @<파일> -H @-`로 하고, 토큰과 이름, 이벤트 이름 헤더는 0001처럼 stdin으로 넘긴다. 이벤트 이름은 hook이 `^[a-z_]{1,64}$`로 먼저 걸러, 맞지 않으면 빈 이름을 보낸다(헤더 주입 방지. 서버는 R125로 거부한다). 응답 처리는 R140이다. 200이 아닌 응답에서 POST를 다시 보내지 않고 `GET /v1/verdict` 조회로 넘어가는 이유는 둘이다. 413처럼 서버가 거부를 기록한 경우는 조회가 그 거부를 돌려주고, 5xx처럼 판정이 없는 경우는 R81의 180초가 runner를 지우므로 허용 없이 step이 돌 경로가 없다(fail closed). 큰 본문을 거듭 보내지도 않는다. 요청은 `host.docker.internal`로 가서 Docker Desktop VM 경계를 넘어 호스트의 loopback listener에 닿는다(0001 Design job 확인 서버). 요청 하나의 시한(`CHILD_MAX` 5초)은 POST에도 같다. 시한을 넘긴 POST는 연결 실패로 보고 2초 뒤 다시 보낸다. 서버는 그때 잘린 본문을 기록하지 않으므로(R137) 다시 보낸 보고가 첫 보고로 판정된다(Areas of concern 14). 나머지(조회 간격, trap, 거부 때 Worker kill, 허용 때 토큰 파일 삭제)는 0001과 같다.
- 판정과 AWS: 판정은 브랜치를 보지 않으므로 main이 아닌 job도 돈다(R141). AWS Role은 관리자 신뢰 정책이 main만 허용하므로 그런 job은 Role을 받지 못한다(R142, 0001 INTENT Problem). main job이 Role을 받는 것(R143)과 함께 보아, R142의 실패가 OIDC 경로의 고장이 아니라 신뢰 정책의 거부임을 가른다. 스케일러는 자격을 넣지 않는다(0001 R12 그대로).

**정리와 대조(R148, R149)**: 조직 runner 목록에는 이 스케일러가 만들지 않은 조직 runner도 있다. 이름 꼴과 `GetRunnerByName`의 scale set ID가 둘 다 맞아야 지우므로 다른 runner는 건드리지 않는다(0001 R17의 규칙). 목록 endpoint는 `GET /orgs/{org}/actions/runners`이고 query는 `name`("The name of a self-hosted runner."), `per_page`(최대 100), `page`이며, runner마다 필수 `name`과 `busy`가 있다(확인함: https://docs.github.com/en/rest/actions/self-hosted-runners "List self-hosted runners for an organization"). 시작 정리(R148)는 0001처럼 모든 페이지를 읽고 페이지 수 상한(`maxRunnerPages`)을 둔다. 30초마다의 대조(R149)는 조직 전체 목록을 읽지 않고 판정 없는 runner마다 `name`으로 그 runner만 조회한다. 조직에 다른 runner가 많아도 호출 수는 판정 없는 runner 하나당 30초에 한 번이다. `name`이 정확히 일치하는 것만 돌려주는지는 문서에 없으므로 결과에서 이름을 다시 비교한다. runner 이름은 `<label>-` 뒤 8자 임의값이고, 조직 안에서 유일해야 하는지는 Areas of concern 8이다.

**delete-scaleset(R150, R151)**: 설정 파일을 읽어 URL이 같은 target을 찾고, 설치 조회(R108)로 설치 ID와 권한을 확인한 뒤, runner group 조회(R111)로 찾은 group에서 `scaling.label` 이름의 scale set과 그 runner 등록을 지운다. 이 명령은 한 번 실행하고 끝나므로 R24의 백오프를 쓰지 않는다. 어느 조회든 실패하면 아무것도 지우지 않고 그 오류를 stderr에 쓰고 1로 끝난다.

**서비스 응답 확인(R153)**: v0.5.0은 poll 시도 하나를 `MessagePollTimeout`(2분)으로 끊고 실패한 시도를 재시도 설정(최대 4번 더)으로 다시 보내며 시도마다 새 2분을 준다(확인함: v0.5.0 `session_client.go` 103~114행 주석, `http_client.go` 73행). 그래서 0001의 끊지 않는 poll은 연결은 되지만 답하지 않는 서비스에서 최대 약 10분 넘게 이어질 수 있어, INTENT의 약 6분을 넘는다. 건강한 서비스는 빈 poll을 약 50.3초 붙잡는다(확인함: 0001 Design 스케일링 흐름 2, 끊지 않은 poll 18번). 그래서 끊지 않는 poll에 70초 시한을 두고, 그 시한으로 끝나면 서버가 답하지 않은 것으로 본다. 감지는 5분 뒤 첫 poll(10초 간격이므로 5분~5분 10초) + 70초로 6분 20초 안이다(R162의 6분 30초). 서버가 70초 넘게 붙잡은 정상 poll은 새 세션 하나로 끝난다(R25의 백오프 1초부터). 로그는 WARN `message poll got no answer`와 경과 시간이다.

**status(R152)**: status JSON은 `{"state","maxRunners","targets":[{"url","busy","idle","startFailures","lastStartError","lastStartErrorAt"}],"lastError","lastErrorAt"}`이다. target 줄은 0001의 pool 줄과 같은 꼴이고 이름 대신 target URL을 쓴다. `전체` 줄의 `<n>`과 `<m>`은 target 줄의 `busy`, `idle` 합이다. `service stop`은 state만 읽으므로 바뀌지 않는다.

**오류 범위**: 어느 target의 영구 오류든(R51) 서버 전체가 오류 drain에 들어간다. 0001이 정하지 않고 남긴 범위(0001 Areas of concern 2)를 이렇게 정해, target을 더할 때 오류 처리를 다시 설계하지 않게 한다. status, drain, 오류 대기가 서버에 하나씩이므로 target마다 상태를 나누는 코드가 필요 없다. 대신 한 target의 설정 오류가 다른 target도 멈춘다(Areas of concern 12).

**시작 순서**: 잠금(R78) → status socket(R60) → 설정 읽기와 검증 → Docker 준비 → 정리(R16) → 설정 오류면 오류 대기(R28, R30, R105~R107) → Docker 노출 확인(R79) → 이미지 확인 → Docker 용량 확인(R118~R120) → target마다: 설치 조회(R108~R110) → runner group(R111) → 정리(R148) → scale set(R112) → `SetSystemInfo` → 첫 세션. 설치 조회, runner group 조회, `/info`는 시작 단계 호출이라 일시 오류면 R24로 다시 시도하고 영구 오류면 오류 대기(R46)다. R148는 0001 R17처럼 시작을 막지 않는다.

**설정 파일**(TOML, 모르는 키 거부. 0001에서 바뀌는 키만 적는다. 나머지 키와 허용 범위는 0001 설정 표 그대로):

| 키 | 기본값 | 허용 범위 / 설명 |
|---|---|---|
| `github.app-client-id`, `github.app-private-key-file` | (필수) | 0001과 같음 |
| `github.app-installation-id` | (없앰) | 있으면 R107. 설치 ID는 R108로 조회 |
| `runner.cpus`, `runner.memory`, `runner.pids` | 1, `1g`, 512 | 0001 `pools.runner-*`의 범위 |
| `buildkit.cpus`, `buildkit.memory`, `buildkit.pids` | 2, `2g`, 1024 | 0001 `pools.build-*`의 범위 |
| `scaling.label` | `linux-arm64-deploy` | 0001 `pools.name`의 규칙. scale set 이름 = 라벨 = `runs-on` |
| `scaling.max-runners` | 2 | 1 이상, 상한 없음. 서버 전체의 runner 수 한도 |
| `[[targets]]` | (필수) | 이번 버전은 정확히 1개(R105) |
| `targets.url` | (필수) | `https://github.com/<조직>`, 조직 이름은 `[A-Za-z0-9-]+`(R106) |
| `targets.runner-group` | `""` | 빈 값이면 ID 1, 아니면 1~255자의 group 이름(R111) |
| `targets.idle` | 0 | 0 이상 `max-runners` 이하 |
| `[[pools]]`와 그 안의 모든 키 | (없앰) | 있으면 R107 |

- 0001 설정을 고르지 않은 이유: 자동 변환(호환)은 `allowed-workflows`와 `allowed-events`를 조용히 무시하게 되는데, 이는 보안 설정이 운영자 모르게 넓어지는 것이다. 그래서 R28의 오류 대기에 들어가 고칠 방법을 오류로 알린다(R107). 두 설정이 막던 경우가 열리는 것은 Areas of concern 4다. 문구(로그와 status, 영어): `pools: [[pools]] is the 0001 format; move max to [scaling] max-runners, idle to [[targets]] idle, runner-*/build-* to [runner]/[buildkit] cpus, memory, pids, name to [scaling] label, and set [[targets]] url = "https://github.com/<org>"; allowed-workflows and allowed-events no longer exist`, `github.app-installation-id: no longer used; the installation is looked up for each target`. 예시 파일(`config.example.toml`)은 자리표시자만 둔다.
- 기본값은 0001과 같다(결정: 2026-11-24, 사용자, 기본 max 2, runner 1 CPU·1GB, BuildKit 2 CPU·2GB, 모두 설정으로 넓힘).

**나중에 개인 계정 저장소를 더할 때**: 저장소 target은 위 target의 네 메서드를 저장소 값(등록 URL `https://github.com/<owner>/<repo>`, 설치 조회 `GET /repos/{owner}/{repo}/installation`, runner 목록 `GET /repos/{owner}/{repo}/actions/runners`, 권한 `administration: write`)으로 채운 종류 하나다. 저장소 설치 조회도 JWT로 부른다(확인함: 같은 apps 문서 "Get a repository installation for the authenticated app"). 동시 실행 한도, runner 표, 판정, 오류 범위는 target 수와 관계없이 이미 서버 전체로 설계했다. 그때 바뀌는 것은 R105(개수)와 R106(URL 꼴)의 검증, 저장소 종류의 구현, 그리고 실제 시험이다. 그 변경에서 정할 것은 Areas of concern 12다.

**나중에 Linux로 옮길 때**: OS마다 다른 코드는 지금 `internal/service`와 `internal/status`에만 있다. `internal/service`는 `daemon_darwin.go`(launchd와 kqueue `EVFILT_PROC`로 종료 감시)와 `daemon_other.go`(`//go:build !darwin`)로, `internal/status`는 `peer_darwin.go`(`LOCAL_PEERPID`로 socket 상대 pid)와 `peer_other.go`(`//go:build !darwin`)로 나뉜다(확인함: 2026-11-26 `GOOS=darwin`과 `GOOS=linux`의 `go list -f '{{.ImportPath}} {{.GoFiles}}' ./...` 비교에서 두 패키지의 이 두 파일만 다름). Linux에서는 `_other.go` 자리에 `SO_PEERCRED`, pidfd나 `waitid`, systemd unit을 같은 작은 인터페이스(`status`의 `peerPID`, `service`의 `Daemon`, `ExitWatch`, 서비스 관리자) 뒤에 더한다. OS별 파일을 더하는 것은 막지 않고, R155가 Linux 빌드와 vet이 깨지지 않게 지킨다. 그 밖에 Linux에서 다를 수 있는 것은 Areas of concern 13이다. 잠금(`flock`), 상태 폴더(`~/.local/state/actions-runner-scaler`), Docker 기본 socket(`unix:///var/run/docker.sock`)은 같은 코드로 Linux에서도 빌드된다. Linux 실행과 시험은 이번 범위 밖이다.

**테스트**
- 단위 테스트는 0001처럼 가짜 scaleset 클라이언트, 가짜 REST, 가짜 Docker로 한다. 판정(R123~R132)은 이벤트 이름과 payload 표로 시험한다: 같은 저장소 PR 허용, fork PR 거부, head.repo null 거부, 최상위 `repository.id`가 다른 PR 거부, `head.repo.fork`가 true이고 ID가 같은 PR 허용, 이름이 PR 계열이 아닌데 `pull_request`가 있는 이벤트의 비교, `pull_request` 없는 PR 계열 이벤트 거부, 같은 저장소 `workflow_run` 허용과 fork `workflow_run` 거부, `workflow_run` 없는 `workflow_run` 이벤트 거부, `issue_comment` 허용, 2^53을 넘는 같은 ID의 허용과 1만 다른 ID의 거부, 소수·음수·0·문자열 ID 거부, 배열·빈 본문·뒤에 값이 더 있는 본문·32MiB 초과·잘못된 이벤트 이름 거부, 판정 중 REST 호출 0번. job 확인 서버(R131~R138)는 405, 본문을 읽지 않는 401(본문 읽기 횟수를 세는 reader로 확인), 413과 기록된 거부, 본문 도중 연결이 닫히거나 `Content-Length`보다 적게 온 요청의 무판정·무기록과 그 뒤 온전한 보고의 판정, 두 번째 보고의 같은 판정, drain 정리 뒤 `pending`, 첫 POST의 busy 시각을 시험한다. hook(R139, R140)은 가짜 서버로 200, 401, 405, 413, 500, 연결 실패 응답마다의 다음 요청을 시험한다.
- 실제 시험(R113, R141~R147, R156~R165)은 `integration` 태그나 사용자 승인 아래의 수동 실행으로 하고 PLAN에서 절차를 정한다. 순서: 조직 설치와 등록 확인 → 판정(R141~R147) → 동시 실행(R156~R158) → 서비스(R159~R161) → 서비스 무응답(R162) → 위 시험 전체의 로그 확인(R163~R165) → 마지막에 재부팅(0001 R19, INTENT 제약에 따라 다른 모든 시험 뒤). 시험 저장소와 fork 준비는 Areas of concern 10.
- 판정 실제 시험의 기록: 0001의 `--record` 이미지 hook은 이벤트 파일에서 R126과 R127의 각 비교 결과(같음, 다름, 없음, null)만 기록하고 ID 값과 이름은 기록하지 않는다. fork와 같은 저장소의 `pull_request`, `pull_request_target`, `workflow_run`을 하나씩 본다.
- R157의 시험 job은 step 둘이다: `sleep 120`, 그리고 `busybox` 기반 이미지(압축 크기 10MB 이하)에 파일 하나를 더하는 Dockerfile의 push 없는 `docker buildx build`. 이 시험은 동시 실행과 한도를 확인하는 것이고 호스트 크기에 맞춘 것이 아니다. 그동안 `docker stats`로 본 실제 사용량을 기록한다.
- R163의 관찰: 스케일러 로그의 poll 시작("Getting next message")과 끊긴 poll의 시각으로 poll 구간을 만들고, `gh run view --json jobs`의 job `started_at`, `completed_at`이 든 구간이 10초로 끊긴 poll인지 본 뒤, 그 job ID의 `JobStarted`, `JobCompleted` 로그가 그 뒤 poll의 응답에서 나왔는지 대조한다. 판정이 `JobStarted`에 기대지 않으므로 메시지를 잃어도 job 실패로는 드러나지 않고 이 대조로만 드러난다.

## How each constraint in the intent is met

| Constraint | Met by |
| :---- | :---- |
| 조직 수준에 등록하고 조직의 runner group 설정이 저장소를 정한다 | R105, R106, R108, R111, R112, R113, R156. 설계: 구성 요소(target, runner group) |
| 조직의 공개 저장소도 쓴다(runner group에서 공개 저장소 허용 필요) | 설계: 구성 요소(runner group). group 설정은 조직 owner의 일, Areas of concern 3, 7 |
| fork pull request의 job을 거부한다 | R123~R129, R139, R140, R145~R147. 설계: 판정. 검증은 Areas of concern 1, 2, 10 |
| 라벨은 `linux-arm64-deploy`이고 다른 job도 거부하지 않는다 | R112, R129. 설계: 설정 파일(`scaling.label` 기본값) |
| 서버 전체에 동시 실행 한도 하나 | R114~R116, R119, R120, R157, R158. 설계: 동시 실행 한도 |
| 개인 계정 저장소를 나중에 다시 설계하지 않고 더할 수 있다 | R108. 설계: target, 나중에 개인 계정 저장소를 더할 때, 오류 범위. Areas of concern 12 |
| Linux를 나중에 다시 설계하지 않고 더할 수 있다 | R155. 설계: 나중에 Linux로 옮길 때. Areas of concern 13 |
| macOS(launchd, Docker Desktop)에서 만들고 시험한다 | R159~R165, 0001 R44. 설계: 테스트 |
| AWS 접근은 관리자 OIDC 신뢰 정책이 맡고 스케일러는 자격을 넣지 않는다 | 0001 R12, R142, R143. 설계: 판정과 AWS. Areas of concern 4 |
| job이 자기 runner의 자격과 JIT 설정을 읽을 수 있는 것은 받아들인 위험 | 설계: 판정(fork 거부). 0001 Areas of concern 18 그대로. 받아들인 이유 중 "쓰기 권한이 있는 사람만 fork가 아닌 job을 시작한다"가 공개 저장소에서 맞지 않는 경우는 Areas of concern 3 |
| 0001의 격리 보장을 모두 지킨다 | 0001 R32, R33, R36, R43, R59, R62, R63, R70~R73, R75, R76, R79 그대로, R121, R122. 설계: 용어(그 밖의 0001 요구사항은 그대로) |
| 관리자 접근 제한을 우회하지 않고 클러스터 안에 runner를 두지 않는다 | 0001 R8, R12 그대로. 설계: 판정과 AWS |
| Go, `actions/scaleset`, 개인 계정 소유 GitHub App을 유지한다 | R108, R110. 설계: 구성 요소(target, 설치 ID 조회, App 권한). Areas of concern 5, 6 |
| 재부팅 시험은 다른 모든 결과가 된 뒤 마지막에 한다 | 0001 R19. 설계: 테스트(순서) |
| 저장소에 내부 이름과 비밀 값을 넣지 않는다 | 0001 R31. 설계: 설정 파일(자리표시자 예시), 테스트(기록 이미지는 비교 결과만) |

## Open questions from the intent

- 서버 전체 한도는 runner 수인가 CPU·메모리 합인가, 어떤 기본값이 바뀌고 이 Mac에서 저장소 5곳 결과를 어떻게 확인하는가? → 답: runner 수(`scaling.max-runners`, 기본 2)다(설계: 동시 실행 한도). 기본값은 바뀌지 않는다(runner 1 CPU·1GB, BuildKit 2 CPU·2GB). job마다의 CPU·메모리는 설정이고 빌드가 더 필요하면 기본값을 올린다. 저장소 5곳 결과는 `max-runners = 5`와 가벼운 시험 job으로 확인한다(R157, R158). 이 확인은 동시 실행과 한도를 보는 것이고 이 Mac의 크기에 맞춘 것이 아니며, 스케일러는 호스트 크기를 가정하지 않는다. 상한 합이 Docker가 보고한 값을 넘는 설정은 시작 경고로 드러낸다(R119, R120).
- 조직 권한 "Self-hosted runners: Read and write"는 누가 더하고 App을 조직에 설치하며, 그 권한으로 라이브러리 호출이 되는가? → 답: App 소유자인 사용자가 App 설정에 권한을 더하고, 조직 owner가 설치(또는 바뀐 권한 승인)를 한다. ARC 문서상 조직 수준 등록에는 이 권한과 Metadata 읽기면 되고 Administration은 저장소 범위에만 필요하다(확인함, 설계: App 권한). 스케일러는 시작 때 이 권한이 설치에 있는지 확인한다(R110). 우리 호출 전부가 되는지의 실제 확인은 넘김: Areas of concern 5.
- 0001 설정 형식(pool마다 저장소 URL과 workflow 허용 목록)은 어떻게 되는가? → 답: 호환하지 않고 오류 대기에서 고칠 키를 알린다(R107, 설계: 설정 파일). `[[pools]]`는 `[scaling]`, `[[targets]]`, `[runner]`·`[buildkit]` 키로 옮기고, 허용 목록은 없앤다. 바꾸거나 없애는 0001 R은 설계의 "바꾸거나 없애는 0001 요구사항"에 모두 적었다. 허용 목록이 막던 경우가 열리는 것은 넘김: Areas of concern 4.
- 0001이 넘긴 다섯 항목 → 답: INTENT의 정리대로다. 기존 workflow의 `runs-on` 변경과 main workflow의 첫 `kubectl apply -f`는 범위 밖인 workflow 이전에 속한다. reusable workflow 허용 규칙은 허용 목록이 없어져 필요 없다(R129). `environment:` job은 범위 밖이다. 빌드 메모리 측정은 하지 않고 job마다의 제한은 기본값이 있는 설정으로 남긴다(R121, R122).
- fork 거부가 fork pull request가 일으킨 `pull_request_target`과 `workflow_run` run도 덮는가? → 답: 덮는다. 최상위 `pull_request`가 있으면 head, base, run 저장소의 ID로(R126), 최상위 `workflow_run`이 있으면 `workflow_run.head_repository`, `workflow_run.repository`, run 저장소의 ID로(R127) 가르고, 실제 실행으로 확인한다(R146, R147). 같은 저장소에서 일으킨 두 이벤트의 run은 허용한다. 두 이벤트 모두 GitHub가 "Running untrusted code on the ... trigger may lead to security vulnerabilities."라고 경고한다(확인함: events-that-trigger-workflows 두 절). 남은 미확인(실제 값, `pull_request_target`이 일으킨 `workflow_run`)은 넘김: Areas of concern 2.

## Areas of concern

1. 판정은 hook이 보낸 이벤트 파일을 믿는다. runner가 hook handler 직전에 서버 job 메시지의 값으로 그 파일을 쓰고, hook이 job의 첫 pre-job step이며, hook 환경에 `GITHUB_EVENT_NAME`과 `GITHUB_EVENT_PATH`가 있고 workflow `env:`는 들어가지 않는다는 것은 확인했다(확인함: actions/runner v2.338.2 d1eb12d 소스, run-scripts 문서. 설계: 판정의 "이벤트 파일을 믿는 근거"). 남은 미확인은 셋이다. (1) 우리 runner 이미지의 실제 실행에서 그 파일에 R126, R127의 값이 오는지(R145~R147의 실제 시험에서 비교 결과만 기록해 본다). (2) 우리 runner 이미지의 hook 환경에 `GITHUB_EVENT_NAME`과 `GITHUB_EVENT_PATH`가 실제로 있는지(R145~R147의 기록 이미지 시험에서 두 변수의 존재를 다시 본다). (3) runner를 올린 뒤에도 `WriteWebhookPayload`가 hook handler 전에 불리고 hook이 첫 pre-job step인지. runner 버전을 올릴 때마다 위 소스 위치를 다시 본다. Owner: 사용자.
2. 이벤트 파일의 fork 경우 값: 다음은 미확인이고 실제 실행으로 본다. (1) fork PR의 `pull_request`와 `pull_request_target` 이벤트 파일에 `pull_request.head.repo.id`와 `pull_request.base.repo.id`가 모두 오고 서로 다른지. (2) fork PR의 run이 일으킨 `workflow_run`의 `workflow_run.head_repository.id`가 fork의 ID인지. (3) `pull_request_target`(fork PR)의 run이 일으킨 `workflow_run`은 그 run이 기본 브랜치에서 돌았으므로 `head_repository`가 base 저장소여서 R127을 통과할 수 있다(추론: actions/checkout f548e57 `unsafe-pr-checkout-helper.ts`의 주석). 통과하면 fork의 입력이 `pull_request_target` run의 artifact를 거쳐 우리 runner에 닿는다. (4) 지운 fork의 `head.repo`가 null로 오는지(낮은 우선순위. null은 거부되므로 안전한 쪽이다). (5) runner는 수를 double로 들고 있다가 직렬화하므로(확인함: actions/runner v2.338.2 `NumberContextData.cs`) 2^53을 넘는 ID는 이벤트 파일에서 정밀도를 잃어 서로 다른 ID가 같게 쓰일 수 있다. 지금 저장소 ID는 2^53보다 훨씬 작다(추론: 공개 저장소 ID의 자릿수). 관찰이 R126~R128의 가정과 다르면 SPEC을 고친다. Owner: 사용자.
3. 공개 저장소에서 쓰기 권한이 없는 사람이 일으키는 job(결정: 2026-11-26, 사용자, GitHub 설정에 맡김). 스케일러는 fork PR(R126, R127) 밖에서 job을 일으킨 사람의 권한으로 거부하지 않는다. ARC와 terraform-aws-github-runner에도 그런 코드는 없다(확인함: ARC eca1a62와 terraform-aws-github-runner 클론의 grep). GitHub는 "self-hosted runners should almost never be used for public repositories on GitHub"라고 적는다(확인함: https://docs.github.com/en/actions/reference/security/secure-use). 이 위험을 줄이는 GitHub 쪽 설정은 다음과 같다. runner group의 공개 저장소 허용은 기본으로 꺼져 있다(확인함: https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access "By default, only private repositories can access runners in a runner group"). fork PR의 workflow run은 쓰기 권한이 있는 maintainer의 승인을 요구하게 할 수 있다(확인함: https://docs.github.com/en/actions/how-tos/manage-workflow-runs/approve-runs-from-forks "Workflow runs triggered by a contributor's pull request from a fork may require manual approval"). Actions 정책의 workflow execution protections는 actor rule(누가)과 event rule(어떤 이벤트)로 run을 막는다(확인함: https://docs.github.com/en/organizations/managing-organization-settings/actions-policies/workflow-execution-protections "Event rules control which events are permitted, such as `push`, `pull_request`, `pull_request_target`, and `workflow_dispatch`."). 그 문서는 공개 저장소의 `pull_request_target`을 막는 기본 정책이 2026-12-02부터 적용된다고 적는다(확인함: 같은 문서). 그 기능을 Free plan 조직이 쓸 수 있는지는 문서에 없다(미확인). 이 결정 아래 열린 경로는 다음과 같다. (a) fork PR에 단 `issue_comment`(`/deploy` 같은 ChatOps)로 PR head를 checkout하는 workflow: payload에는 `issue.pull_request`의 URL만 있고 head 저장소가 없어(확인함: github/rest-api-description c9c95fe의 issue comment webhook 스키마) 이벤트 파일로 가를 수 없다. (b) 두 단계 `workflow_run` 연쇄: fork PR → GitHub 호스팅 runner의 run A → A가 일으킨, 기본 브랜치에서 도는 run B → B가 일으킨 run C. C의 `workflow_run`은 B이고 B의 `head_repository`는 base 저장소라 R127을 통과하며(추론), C가 A의 artifact를 내려받으면 fork의 내용이 우리 runner에서 쓰인다. (c) 같은 저장소 PR에 외부인이 남긴 `pull_request_review`: head와 base가 같아 허용되고, 리뷰 본문은 외부인이 정한다. (d) `watch`, `fork`, `discussion_comment`: 누구나 일으킬 수 있고 기본 브랜치의 workflow가 돈다(추론: events-that-trigger-workflows의 각 활동과 공개 저장소의 기본 권한). 일으킨 사람의 쓰기 권한은 `author_association`으로 정할 수 없고(그 값은 관계를 나타냄. 확인함: OpenAPI `author-association` enum, GraphQL `CommentAuthorAssociation` 설명) 정확히 보려면 REST 협업자 권한 조회(`GET /repos/{owner}/{repo}/collaborators/{username}/permission`)가 필요하다. 이 설계는 그 조회를 하지 않는다(R123). 그래서 INTENT가 자격 노출을 받아들인 이유 중 "쓰기 권한이 있는 사람만 fork가 아닌 job을 시작한다"는 공개 저장소에서는 위 경로에서 맞지 않는다. Owner: 사용자 (결정: GitHub 설정에 맡김).
4. 0001의 최신 run 조건(0001 판정 8)과 `allowed-events` 제한(0001 판정 3, `push`, `workflow_dispatch`, `schedule`만 허용해 `pull_request_target`과 `workflow_run`을 막음)이 이 변경에서 빠진다. 그 결과 다음이 허용된다. (1) 쓰기 권한이 있는 사람의 옛 main run 재실행: 재실행은 원래 커밋 SHA와 ref로 30일까지 된다(확인함: 0001 Areas of concern 19에 인용한 re-run 문서). main ref이므로 관리자 신뢰 정책의 main 조건을 통과해 AWS Role을 받고 옛 코드를 배포할 수 있다(추론. 신뢰 정책이 옛 커밋을 가르는지는 미확인, 0001 Areas of concern 19 (6)). (2) 같은 저장소의 `pull_request_target`과 `workflow_run`: 기본 브랜치 ref로 돈다(확인함: 설계 판정의 이벤트 파일로 판정하는 이유). 기본 브랜치가 main이면 그 run이 Role을 받는지는 신뢰 정책의 subject 조건에 달렸다(미확인). 최신 run 조건을 되살리려면 같은 workflow의 다른 run을 REST로 조회해야 하는데, 그 값은 이벤트 파일에도 `JobStarted`에도 없어(추론) "판정하려고 GitHub에 요청을 보내지 않는다"(R123)와 맞지 않는다. 선택지(사실만 적음): (a) 받아들인다. (b) REST 조회를 되살리고 R123을 바꾼다. 0001처럼 Actions 읽기 권한, job마다의 조회, 조회 실패 거부(0001 R65), 속도 제한이 다시 생긴다. 이벤트 제한만은 보고된 이벤트 이름으로 REST 없이 할 수 있지만, 서버 전체에 목록 하나라 "fork pull request만 거부" 결정과 INTENT의 "어느 브랜치, pull request, 수동 실행이든 돈다"를 좁힌다. (c) GitHub 쪽 통제: workflow execution protections의 event rule로 저장소별로 `pull_request_target`, `workflow_run`을 막는다(확인함: Areas of concern 3의 문서. Free plan에서 되는지는 미확인). environment 보호 규칙(필수 검토자)은 `environment:` job이 INTENT 범위 밖이라(OIDC subject가 달라짐) 이 변경에서는 쓰지 못한다. 재실행 자체를 막는 GitHub 설정은 찾지 못했다(미확인). 다른 스케일러도 재실행이나 이벤트 종류로 막지 않는다. runner-up은 fork에서 온 job만 거부하고 "It allows every other job from the repository itself"라고 적으며, 재실행은 "Rerunning a failed job is left to GitHub and the repository"라고 적는다(확인함: runner-up 013cc74 `README.md` "Pull requests from forks" 절, `VISION.md`). ARC(eca1a62)는 이벤트 이름을 지표 라벨에만, elastic-fruit-runner(bcb9c8e)는 실행 기록에만 쓰고, runscaler(6f33444)와 runpool(f1a2ab3)에는 관련 코드가 없다(확인함: 각 클론의 grep). Owner: 사용자 (결정: (a) 받아들임. 쓰기 권한이 있는 사람의 재실행과 같은 저장소의 `pull_request_target`, `workflow_run`은 허용되고, 배포 통제는 저장소 쓰기 권한과 관리자 신뢰 정책에 맡긴다).
5. App에 Organization permission "Self-hosted runners: Read and write"를 더하는 것은 App 소유자인 사용자가, 조직 설치와 바뀐 권한의 승인은 조직 owner가 한다. 이 권한으로 scale set 생성, 세션, JIT, `RemoveRunner`, 조직 runner 목록과 이름 조회(R148, R149), `delete-scaleset`이 모두 되는지는 ARC 문서와 REST 문서 도입부에서 본 것이고 실제 호출로는 미확인이다. 조직 runner 목록 문서의 endpoint 절은 fine-grained 권한을 따로 적지 않는다(확인함: self-hosted-runners 문서). `name` query가 정확히 일치하는 runner만 돌려주는지도 문서에 없다(미확인, 결과에서 이름을 다시 비교함). App이 설치되지 않은 조직에 대한 `GET /orgs/{org}/installation`의 응답 코드는 문서에 200만 있으므로(R109의 404), 미설치 조직의 응답 코드는 실제 호출로 확인한다. 이미 설치된 다른 곳의 권한 승인도 같이 요구될 수 있다. Owner: 사용자.
6. 쓰지 않게 된 App 권한: 이 변경의 호출은 Repository permission "Administration: Read and write"(저장소 범위 등록용)와 "Actions: Read"(0001의 run 조회용)를 쓰지 않는다(설계: App 권한). 두 권한을 App에서 뺄지, 나중의 개인 계정 저장소 target(Administration 필요)을 위해 둘지 정한다. 빼면 그 App을 설치한 다른 곳에도 영향이 있다. Owner: 사용자.
7. runner group: 공개 저장소를 쓰려면 조직 owner가 그 group에서 공개 저장소를 허용해야 한다(Areas of concern 3). 기본 group의 ID가 1이라는 것은 ARC의 관례이고 문서에는 없다(미확인). REST의 group 생성 기본값 `visibility: all`과 관리 문서의 "only private repositories" 기본이 서로 다른 축이라, 실제 group의 저장소 접근과 공개 허용은 시험 전에 조직 설정에서 본다. 서버가 다른 group에 같은 이름의 scale set을 만들게 두는지도 미확인이다. 두면 `targets.runner-group`을 바꿀 때 옛 scale set을 먼저 지워야 하고(설계: runner group), 두지 않으면 R112의 생성이 실패해 오류 대기에 들어간다(추론). Owner: 사용자.
8. runner 이름이 조직 안에서 유일해야 하는지는 미확인이다. 이름은 `<label>-` 뒤 소문자 16진수 8자 임의값이다. 겹쳐서 JIT 발급이 실패하면 R64의 시작 실패로 세고 다음 poll에 새 이름으로 다시 시도한다. 조직에 같은 꼴 이름의 다른 runner가 있어도 R148는 scale set ID가 다르면 지우지 않는다. 실제 실행에서 겹친 이름의 JIT 발급 결과를 볼지 정한다. Owner: 사용자.
9. fork PR이 잇달아 오면 job마다 runner 자리 하나를 거부될 때까지 쓴다(판정 1초 안, R131. 거부 뒤 컨테이너가 없어질 때까지 최대 40초, R145). 공개 저장소에서는 누구나 fork PR을 열 수 있으므로, 그동안 다른 저장소의 job이 GitHub 큐에서 기다릴 수 있다(가용성). GitHub의 fork run 승인 설정(Areas of concern 3)을 켜면 승인 전에는 job이 runner에 오지 않으므로 줄어든다. Owner: 사용자.
10. 시험 저장소: R156~R158은 서로 다른 조직 저장소 5곳 이상과 그 사이에 runner group에 더할 저장소 하나가, R145~R147은 다른 계정의 fork와 pull request가 필요하다. 공개 저장소에서 R146을 하려면 2026-12-02 뒤에는 `pull_request_target`을 허용하는 event 정책이 필요하다(Areas of concern 3의 문서). 기존 저장소를 쓸지, 조직에 임시 저장소를 만들고 시험 뒤 지울지, fork를 어느 계정으로 만들지, 시험 workflow를 각 main에 쓰는 것은 모두 사용자 승인 사항이다. Owner: 사용자.
11. 서비스 무응답 감지(R153, R162): 70초 시한은 건강한 서버가 빈 poll을 약 50.3초 붙잡는 관찰(18번)에 기댄다. 서버가 붙잡는 시간을 늘리면 5분마다 세션을 다시 만들게 된다(로그로 드러남). 실제 실행에서 "연결은 되지만 답하지 않는" 상태를 만드는 방법(호스트 방화벽 규칙으로 응답 패킷을 버림, 가로채는 프록시 등)은 이 Mac의 설정을 바꾸므로 사용자가 정한다. 연결 자체를 막으면 10초 poll이 바로 일시 오류가 되어(0001 스케일링 흐름 2) 6분 경로를 시험하지 못한다. Owner: 사용자.
12. target이 여럿일 때(이번 버전은 1개): (1) 각 scale set이 `maxCapacity` = `max-runners`로 배정받아 배정 합이 한도를 넘으면, 넘친 job은 다른 target이 자리를 비울 때까지 배정된 채 기다린다. (2) 한 target의 영구 오류가 서버 전체를 오류 drain에 넣는다(설계: 오류 범위). (3) 개인 계정 저장소의 기본 runner group ID와 필요한 권한 조합은 미확인이다(actions-scaleset 스킬 references/runtime.md 1절). 개인 계정 저장소를 더하는 변경에서 정한다. Owner: 사용자.
13. Linux로 옮길 때 `host.docker.internal`(Docker Desktop의 이름), loopback에만 listen하는 job 확인 서버에 bridge 네트워크 컨테이너가 닿는지, rootless Docker의 socket 경로, Docker Desktop이 아닌 엔진의 2375 노출 확인은 모두 미확인이다. job 확인 서버의 listen 주소 규칙(0001 `jobcheck.listen`은 loopback만)은 Linux에서 바뀔 수 있다. Owner: 사용자.
14. 이벤트 보고의 전송 시간: hook의 `POST /v1/event`는 `host.docker.internal`로 Docker Desktop VM 경계를 넘어 호스트의 listener에 닿는다. 본문이 클 때(최대 32MiB) 이 경로의 시간은 미확인이다. 요청 하나의 시한 5초를 넘기면 hook은 2초 뒤 다시 보내고(R140), 끝내 보내지 못하면 R81의 180초에 runner가 지워져 job이 실패한다(허용 없이 step은 돌지 않음). 0001에서 실제 실행으로 확인한 판정 경로(거부 때 Worker만 kill, 1.47~3.58초 정지)는 판정 입력만 바뀌고 같은 코드를 쓴다. 실제 시험에서 이벤트 파일 크기와 POST 시간을 잰다. Owner: 사용자.
