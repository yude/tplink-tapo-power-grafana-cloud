# Tapo power collector for Grafana Cloud

TP-Link Cloud から Tapo スマートプラグの電力情報を読み取り、Grafana Cloud の
OTLP/HTTP エンドポイントへ送信する Go 製デーモンです。Kubernetes 上で常駐させる
ことを前提にしています。現行版に Apps Script のコードやランタイム依存はありません。

このアプリケーションは読み取り専用です。電源 ON/OFF、トグル、設定変更などの
デバイス操作 API は実装していません。

## 仕組みと制約

TP-Link が公開している Tapo Open API は提携事業者向けであり、一般ユーザー向けの
認証情報や完全な仕様は公開されていません。本実装は Tapo Android アプリが利用する
クラウド API の公開解析情報に基づく非公式クライアントです。TP-Link による互換性
保証はなく、クラウド側の変更によって動作しなくなる可能性があります。

現行 P110M が利用する Thing API は、一般的な公開 CA ではなく TP-Link Cloud Server
CA が発行した証明書を返します。本アプリはその CA 証明書をバイナリへ組み込み、
接続先を `https://<region>-app-server.iot.i.tplinkcloud.com` に限定したうえで通常どおり
TLS 証明書とホスト名を検証します。証明書検証を無効化するオプションはありません。

運用時は次を推奨します。

- 普段利用する TP-Link ID とは分離した専用 ID を使う。
- 対象機器だけを専用 ID へ共有する。
- Kubernetes Secret と Grafana Cloud token へのアクセスを限定する。
- リスクを許容できない場合は TP-Link へ Tapo Open API の利用を申請する。

## 収集内容

選択したプラグごとに、対応している次の読み取りメソッドを呼び出します。

- `get_energy_usage`: 当日・当月の積算電力量、稼働時間、現在電力
- `get_current_power`: 現在電力
- `get_emeter_data`: 電圧、電流、現在電力など
- `get_emeter_vgain_igain`: 電力計測用ゲイン値
- `get_device_usage`: 当日・7 日・30 日の利用統計
- `get_energy_data`: 時間・日・月単位の履歴

既知の値は SI 単位へ正規化します。未知の数値フィールドも捨てず、フィールドの
JSON パスを `tapo.energy.field` 属性に持つ `tapo_energy_raw_value` として送信します。

| メトリクス | 単位 | 内容 |
| --- | --- | --- |
| `tapo_power_watts` | W | 瞬時電力 |
| `tapo_voltage_volts` | V | 電圧 |
| `tapo_current_amperes` | A | 電流 |
| `tapo_energy_today_watt_hours` | Wh | 当日の積算電力量 |
| `tapo_energy_month_watt_hours` | Wh | 当月の積算電力量 |
| `tapo_energy_total_watt_hours` | Wh | 総積算電力量 |
| `tapo_runtime_today_seconds` | s | 当日の稼働時間 |
| `tapo_runtime_month_seconds` | s | 当月の稼働時間 |
| `tapo_energy_bucket_watt_hours` | Wh | 履歴バケット |
| `tapo_device_online` | 1 | 少なくとも 1 つの読み取りが成功したか |
| `tapo_energy_collection_success` | 1 | 電力収集の成功状態 |
| `tapo_energy_raw_value` | 1 | 未知の数値フィールド |

各系列には機器 ID、機器名、モデル、取得元を属性として付加します。TP-Link ID、
パスワード、Grafana token、MAC アドレスはメトリクスへ送信しません。

通常の収集は既定で 5 分ごと、履歴収集は 24 時間ごとです。起動直後にも履歴を
収集します。履歴収集を止める場合は `HISTORY_INTERVAL=0` と
`HISTORY_ON_START=false` を設定します。

## Grafana Cloud の準備

Grafana Cloud の OpenTelemetry 設定から次を取得します。

1. OTLP endpoint（通常は `https://...grafana.net/otlp`）
2. instance ID
3. `metrics:write` 権限を持つ Cloud Access Policy token

設定した endpoint が `/v1/metrics` で終わっていなければ、アプリがそのパスを追加
します。送信形式は OTLP/HTTP JSON、認証方式は Grafana Cloud が案内する instance ID
と token による Basic 認証です。

## 設定

| 環境変数 | 必須 | 既定値・用途 |
| --- | --- | --- |
| `TAPO_USERNAME` | 必須 | 専用 TP-Link ID のメールアドレス |
| `TAPO_PASSWORD` | 必須 | TP-Link ID のパスワード |
| `TAPO_TERMINAL_ID` | 必須 | 永続化する UUID。Pod 再作成時も変更しない |
| `TAPO_MFA_CODE` | 任意 | 初回端末確認コード。通常は空にする |
| `TAPO_MFA_CODE_FILE` | 任意 | 確認コードを待つファイルのパス |
| `TAPO_MFA_WAIT_TIMEOUT` | 任意 | コード待機時間。既定 `10m` |
| `TAPO_DEVICE_IDS` | 任意 | 対象 Thing ID のカンマ区切り。空なら電力機器を自動選択 |
| `GRAFANA_OTLP_ENDPOINT` | 必須 | Grafana Cloud の OTLP endpoint |
| `GRAFANA_OTLP_INSTANCE_ID` | 必須 | OTLP instance ID |
| `GRAFANA_CLOUD_TOKEN` | 必須 | `metrics:write` token |
| `COLLECTION_INTERVAL` | 任意 | 通常収集間隔。既定 `5m`、最小 `1m` |
| `HISTORY_INTERVAL` | 任意 | 履歴収集間隔。既定 `24h`、`0` で停止 |
| `HISTORY_ON_START` | 任意 | 起動直後の履歴収集。既定 `true` |
| `REQUEST_TIMEOUT` | 任意 | 1 HTTP 要求の期限。既定 `30s` |
| `LISTEN_ADDRESS` | 任意 | probe 用 HTTP アドレス。既定 `:8080` |
| `TZ` | 任意 | 履歴区間のタイムゾーン。既定 `Asia/Tokyo` |

`GET /healthz` はプロセスの生存、`GET /readyz` は直近の収集成功を表します。

### 初回の端末確認

TP-Link はアカウントの 2 段階認証が無効でも、新しい terminal ID の初回ログインに
メール確認を要求することがあります。その場合は確認メールを送信し、
`TAPO_MFA_CODE_FILE` の内容が空でなくなるまで待機します。Kubernetes では Secret の
projected volume をこのファイルへ割り当て、メールで届いたコードを Secret に一時的に
設定します。認証後は同じ `TAPO_TERMINAL_ID` を使い続け、コードを Secret から消して
ください。具体的な手順は k8s リポジトリのマニフェスト README に記載しています。

## ローカルでのビルドと検証

本番 API へ接続しないテストだけを実行します。

```bash
gofmt -w cmd internal
go vet ./...
go test -race ./...
docker build -t tapo-grafana-collector:local .
```

ローカル実行時は上記の環境変数を設定して次を実行します。

```bash
go run ./cmd/tapo-grafana-collector
```

## コンテナイメージ

`main` への push と `v*` tag の push でテスト後に次のイメージを GHCR へ公開します。

```text
ghcr.io/yude/tplink-tapo-power-grafana-cloud:<git-short-sha>
ghcr.io/yude/tplink-tapo-power-grafana-cloud:main
```

リリース tag では semver tag も付与します。最終イメージは static binary だけを含む
scratch image で、UID/GID `65532:65532` として動作します。Kubernetes マニフェストでは
再現性のため `main` ではなく commit SHA tag を指定します。

## 参考資料

- [TP-Link Tapo Product Guide — Tapo Open API](https://static.tp-link.com/upload/soho/2024/202405/20240510/2024%20Tapo%20Product%20Guide_English.pdf)
- [python-kasa energy module](https://github.com/python-kasa/python-kasa/blob/master/kasa/smart/modules/energy.py)
- [TP-Link Cloud API protocol research](https://github.com/piekstra/tplink-cloud-api)
- [Tapo Cloud Thing API protocol research](https://github.com/stoyanov-x/tapo-things-api-hub-research/blob/main/docs/cloud-thing-api.md)
- [Grafana Cloud OTLP documentation](https://grafana.com/docs/grafana-cloud/send-data/otlp/send-opentelemetry-data/)
- [OpenTelemetry OTLP specification](https://opentelemetry.io/docs/specs/otlp/)

本実装は上記のプロトコル情報を参考にした独立実装であり、参照リポジトリのソース
コードは同梱していません。
