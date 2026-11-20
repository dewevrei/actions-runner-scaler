# actions-runner-scaler

GitHub Actions job마다 일회용 self-hosted runner를 Docker 컨테이너로 띄우고, 대기 중인 job 수에 따라 0~N개로 자동 확장하는 스케일러입니다. [`actions/scaleset`](https://github.com/actions/scaleset)을 사용합니다.

> 개발 중이며 아직 동작하지 않습니다.

## 요구 사항

- Go 1.27.3 이상
- Docker

## 빌드

```sh
go build -o bin/actions-runner-scaler ./cmd/actions-runner-scaler
```
