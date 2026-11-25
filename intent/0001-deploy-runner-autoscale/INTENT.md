# Intent: 배포 job용 self-hosted runner 자동 확장

Author: dewevrei. Status: implemented.

호스트 재시작 뒤 5분 안에 다시 도는지는 0002(`intent/0002-org-shared-runner/`)에서 확인한다.

## Problem

팀의 배포 job은 클러스터에 접속하고 이미지를 push해야 하는데, 관리자 정책상 허용된 네트워크에서만 된다(확인함: 시험). AWS 자격은 키 없이 배포 job이 GitHub OIDC로 Role을 받고, Role은 등록된 팀 저장소 네 개의 main만 허용한다(확인함: 관리자 안내문 2026-11-24). 그 네트워크의 호스트에는 runner를 자동으로 띄우는 것이 없어 팀원이 직접 켜야 한다.

## Proposed outcome

- 배포 job이 오면 1분 안에 runner 컨테이너가 받고, job 하나를 끝내면 사라진다.
- 동시 runner는 설정한 최대 수를 넘지 않고, job이 없으면 대기 수만큼만 남는다.
- 시험 저장소 main의 job에서 OIDC로 Role을 받아 클러스터 Pod 목록 조회가 성공한다.
- main 배포 job 안에서 이미지를 빌드하고 push할 수 있다.
- 설정에 적힌 저장소와 workflow의 main job만 돌고, 나머지는 첫 step 전에 실패한다.
- 호스트 재시작이나 비정상 종료 뒤 사람 손 없이 5분 안에 다시 돈다.
- 운영자는 상태(runner 수, 최근 오류)를 보고 스케일러를 멈출 수 있다.
- pool마다 runner 수, CPU와 메모리, 허용 목록을 바꿀 수 있고, pool을 나중에 더할 수 있다.
- Out of scope: 배포 workflow 이전, 나머지 저장소 세 곳, CI pool(모두 다음 INTENT).
- Out of scope: 조직 단위 등록, 여러 호스트, VM 격리.

## Affected users and systems

- 팀의 배포 workflow와 팀원. 이번에는 시험 저장소 한 곳.
- 허용된 네트워크의 상시 가동 호스트(macOS Apple Silicon, Docker Desktop).
- GitHub scale set, GitHub App, OIDC, AWS Role, 레지스트리, 쿠버네티스 클러스터.

## Constraints

- 관리자의 접속 제한을 우회하지 않는다. 이유: 관리자 정책.
- 클러스터 안 runner(ARC 등)는 못 쓴다. 이유: 팀 권한으로 Role, CRD를 만들 수 없다(확인함).
- Go와 `github.com/actions/scaleset`을 쓴다. 이유: GitHub 공식 라이브러리이고 ARC도 쓴다.
- 인증은 개인 계정 GitHub App, 저장소 단위 등록, 시험 저장소부터. 이유: 토큰 갱신을 라이브러리가 맡고 개인 토큰과 분리된다.
- runner 이름과 `runs-on`은 `linux-arm64-deploy`, 추가 라벨 없음. 이유: OS-아키텍처-용도 관례.
- 기본값은 pool마다 최대 2, 대기 0, runner CPU 1과 메모리 1GB, 빌드 컨테이너 CPU 2와 메모리 2GB이고, 모두 설정할 수 있고 코드 상한은 없다(결정: 2026-11-24, 사용자). 이유: 합계가 Docker VM(10 CPU, 7.75GiB)에 든다(확인함).
- runner는 비root, 호스트 Docker 소켓 없음, CPU·메모리 제한으로 돈다. 어느 컨테이너도 privileged를 쓰지 않고, job 코드에 호스트 Docker나 다른 runner를 제어할 길을 주지 않는다. 이유: job 코드가 AWS 자격에 닿고 같은 호스트에 App 키가 있다.
- 빌드는 runner마다 스케일러가 함께 띄우고 지우는 rootless BuildKit 컨테이너가 하고, job은 빌드 요청만 보낸다(결정: 2026-11-24, 사용자).
- `seccomp=unconfined`는 빌드 컨테이너에만 허용하고, 맞춤 seccomp 프로필이 되면 바꾼다(결정: 2026-11-24, 사용자). 이유: rootless BuildKit에 필요하다(확인함: 로컬 시험).
- 스케일러는 자격을 넣지 않고, AWS의 main 제한은 관리자 신뢰 정책이 맡는다(결정: 2026-11-24, 사용자). 이유: 관리자가 키를 발급하지 않는다.
- 배포 job의 이미지 빌드와 push를 범위에 넣는다(결정: 2026-11-24, 사용자). 이유: push가 허용된 네트워크에서만 된다(미확인).
- CI는 나중에 자기 라벨, 설정, 캐시를 가진 별도 pool로 보내고 섞지 않는다(결정: 2026-11-24, 사용자). 이유: main 전용 보장 유지.
- 이번에는 시험 저장소에만 CD pool 하나를 등록하고, 그 main 쓰기는 승인 후, App 설치는 사용자가 시험 전에 한다(결정: 2026-11-24, 사용자). 이유: 다른 팀원 작업과 분리.
- 호스트는 재부팅 뒤 사람 없이 로그인과 Docker가 시작된다(확인함; 잠자기와 정전 복구는 미확인). 이유: 무인 운영의 전제.
- 공개 저장소이고 라이선스, 내부 이름, 비밀 값을 두지 않는다. 커밋, 푸시, GitHub 설정 변경, 실제 시험은 사용자 승인 후에 한다.

## Open questions

- 없음.
