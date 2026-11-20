# Commands
- `integration` 빌드 태그 테스트는 실제 GitHub와 Docker를 호출하므로 사용자 승인 후에만 실행한다.

# Conventions
- 설계 문서는 루트의 `INTENT.md` → `SPEC.md` → `PLAN.md` 순서로 하나씩 만든다. 앞 문서가 `Status: accepted`가 된 뒤에 다음 문서를 쓴다. 동작을 바꾸기 전에 `SPEC.md`와 `PLAN.md`를 읽는다.
- 커밋, 푸시, PR 생성 전에 `git diff --stat`과 메시지를 보여 주고 사용자 승인을 받는다. PR 머지는 사용자만 한다. 커밋 전에 `go vet ./...`와 `go test ./...`를 통과시킨다.

# Gotchas
- AWS 키, GitHub 토큰, GitHub App 개인키의 값은 읽거나 출력하거나 파일, 로그, 커밋에 쓰지 않는다(등록은 사용자가 직접 한다). 팀 내부 이름(클러스터, IAM 정책, 호스트명)도 쓰지 않고, 계정 번호는 `<acct>`로 쓴다.
- 관리자가 건 접속 제한(IAM 정책 등)을 우회하지 않는다. 노트북에서 정상으로 되는 경로만 쓴다.
- GitHub 설정(Environment, 시크릿, 변수, runner, scale set 등록), 원격 저장소 생성, `kubectl apply/delete/patch/scale`, 권한 변경은 사용자 승인 없이 하지 않는다. 시험으로 만든 리소스는 시험 직후 삭제하고 보고한다.
- macOS(Apple Silicon)의 기본 bash는 3.2(연관 배열 없음)이고, zsh는 변수를 단어 분리하지 않는다.
- Docker Desktop의 컨테이너는 linux/arm64다. runner 이미지와 도구는 arm64용이어야 한다.
