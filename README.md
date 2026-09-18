# Tapo energy collector for Grafana Cloud (Google Apps Script)

TP-Link CloudからTapoスマートプラグの電力情報を読み取り、Grafana Cloudへ
OTLP/HTTP JSONで送信するGoogle Apps Scriptアプリケーションです。

このアプリケーションは読み取り専用です。電源ON/OFF、トグル、設定変更などの
デバイス操作APIは実装していません。

## 重要な制約とセキュリティ上の注意

TP-Linkが案内しているTapo Open APIは、一般ユーザー向けに仕様と認証情報が公開
されたAPIではなく、提携事業者向けです。この実装はTapo Androidアプリが利用する
クラウドAPIの公開解析情報に基づく非公式クライアントであり、TP-Linkによる互換性
保証はありません。ファームウェアやクラウド側の変更で停止する可能性があります。

Tapoクラウドには、公開信頼証明書を返すホストと、TP-Link独自CAの証明書を返す
ホストがあります。Google Apps Scriptは独自CAを追加できないため、この実装は
公開信頼証明書を持つ`https://wap.tplinkcloud.com`を初期ホストに使い、
HTTPS証明書を既定で検証します。地域判定APIが`n-`で始まる独自CA側の
ゲートウェイを返した場合は、同一地域の公開信頼証明書側ホストへ正規化します。
ただし、現行P110Mが使うThing APIはTP-Link独自CAの
`https://<region>-app-server.iot.i.tplinkcloud.com`でしか提供されません。接続先を上記の
厳密なホスト名パターンに制限し、`validateHttpsCertificates: false`も指定していますが、
現在のUrlFetchAppはこのホストとのTLSハンドシェイク自体を`SSL Error`で拒否します。
公開CA側の同等APIホストも存在しないため、現時点で純粋なGASだけからP110MのThing API
へ接続することはできません。v0.3.1以降は、この場合に旧APIへフォールバックして
オンライン機器を誤ってオフラインと記録せず、制約を明示するエラーで停止します。
次の運用も強く推奨します。

- 普段利用するTP-Link IDとは分離した、Tapo専用IDを使用する。
- 対象機器だけを専用IDへ共有する。
- Apps Scriptプロジェクトを他人と共有しない。
- リスクを許容できない場合は、この実装を使用せず、TP-LinkへTapo Open APIの利用を申請する。

`TAPO_ALLOW_INSECURE_TLS=true`を設定すると証明書検証を無効化できますが、通常は
設定しないでください。中間者攻撃によるTP-Link ID認証情報の漏えいにつながるため、
旧APIホストを診断する場合だけの設定です。Thing APIの限定的な例外には影響しません。

## 取得するデータ

対応する機器が返す次の読み取り専用メソッドを試します。機種・ファームウェアに
存在しないメソッドは無視し、取得できた形式だけを送信します。
現行P110MではTapoアプリと同じThing APIを優先し、旧クラウドpassthroughが返す
`-20571 Device is offline`をオンライン判定には使いません。

- `get_energy_usage`: 当日・当月の積算電力量、稼働時間、現在電力
- `get_current_power`: 現在電力
- `get_emeter_data`: 電圧、電流、現在電力など
- `get_emeter_vgain_igain`: 電力計測用ゲイン値
- `get_device_usage`: 当日・7日・30日の利用統計
- `emeter.get_realtime`: 旧形式の電圧、電流、電力、積算電力量
- `get_energy_data`: 時間・日・月単位の履歴（手動バックフィル時）
- `emeter.get_daystat` / `get_monthstat`: 旧形式の履歴（フォールバック）

既知の単位はSI単位へ正規化します。未知の数値フィールドも捨てずに
`tapo_energy_raw_value`へ格納し、元のJSONパスを`tapo.energy.field`ラベルに残します。

主なGrafana Cloud上のメトリクスは次のとおりです。

| メトリクス | 単位 | 内容 |
| --- | --- | --- |
| `tapo_power_watts` | W | 瞬時電力 |
| `tapo_voltage_volts` | V | 電圧 |
| `tapo_current_amperes` | A | 電流 |
| `tapo_energy_today_watt_hours` | Wh | 当日の積算電力量 |
| `tapo_energy_month_watt_hours` | Wh | 当月の積算電力量 |
| `tapo_energy_total_watt_hours` | Wh | 機器が返す総積算電力量 |
| `tapo_runtime_today_seconds` | s | 当日の稼働時間 |
| `tapo_runtime_month_seconds` | s | 当月の稼働時間 |
| `tapo_energy_bucket_watt_hours` | Wh | 履歴の時間・日・月バケット |
| `tapo_device_online` | 1 | TP-Link Cloud上のオンライン状態 |
| `tapo_energy_collection_success` | 1 | 収集成功状態 |
| `tapo_energy_raw_value` | 1 | 未知の数値フィールド |

各系列には機器ID、機器名、モデル、取得元をラベルとして付与します。MACアドレスや
アカウント名、認証情報は送信しません。

## Grafana Cloudの準備

1. Grafana Cloud Portalで対象Stackを開きます。
2. OpenTelemetryの設定画面からOTLP endpointとinstance IDを確認します。
3. `metrics:write`スコープを持つCloud Access Policy tokenを発行します。
4. endpointは通常`https://...grafana.net/otlp`という形式です。

このアプリは、GASから扱いやすいOTLP/HTTP JSONを`/otlp/v1/metrics`へ送ります。
低頻度・少量のスマートプラグ収集を前提としています。

## Apps Scriptへの配置

リポジトリを取得し、ローカルで[clasp](https://github.com/google/clasp)を使用します。

```bash
git clone https://github.com/yude/tplink-tapo-power-grafana-cloud-gas.git
cd tplink-tapo-power-grafana-cloud-gas
npm install -g @google/clasp
clasp login
```

Google Apps Scriptでスタンドアロンプロジェクトを作成し、プロジェクト設定から
Script IDを確認します。`.clasp.json.example`を`.clasp.json`へコピーしてScript IDを
設定した後、次を実行します。

```bash
npm test
clasp push
```

`.clasp.json`はGitの除外対象です。実際のScript IDやScript Propertiesを公開
リポジトリへコミットしないでください。

## Script Properties

Apps Scriptの「プロジェクトの設定」→「スクリプト プロパティ」に以下を設定します。
値をソースコードへ直接書かないでください。

| 名前 | 必須 | 内容 |
| --- | --- | --- |
| `TAPO_USERNAME` | 必須 | 専用TP-Link IDのメールアドレス |
| `TAPO_PASSWORD` | 必須 | 専用TP-Link IDのパスワード |
| `TAPO_ALLOW_INSECURE_TLS` | 任意 | 通常は未設定。診断時に証明書検証を無効化する場合だけ`true` |
| `GRAFANA_OTLP_ENDPOINT` | 必須 | Grafana Cloudの`.../otlp` endpoint |
| `GRAFANA_OTLP_INSTANCE_ID` | 必須 | OTLP instance ID |
| `GRAFANA_CLOUD_TOKEN` | 必須 | `metrics:write` token |
| `TAPO_DEVICE_IDS` | 任意 | 収集対象device IDのカンマ区切り。空ならプラグ系すべて |
| `TAPO_INCLUDE_OFFLINE` | 非推奨 | 互換性のため読込のみ。現在は選択した全プラグへ収集を試行 |
| `TAPO_APP_VERSION` | 任意 | API変更時の互換性調整用。通常は設定不要 |

内部的に`_TAPO_`で始まるプロパティへ端末IDとセッショントークンを保存します。
これらを手動編集しないでください。

## 初期設定と実行

Apps Scriptエディタから順に実行します。

1. `validateConfiguration()` — 外部通信なしで設定を検査
2. `initializeTapoSession()` — TP-Link Cloudへログイン
3. `runCollection()` — 1回収集し、Grafana Cloudへ送信
4. Grafana Exploreで`tapo_power_watts`などを確認
5. Apps Scriptエディタ左側の「トリガー」から`runCollection`の時間主導型トリガーを追加

同時実行はScript Lockで抑止します。最小権限を維持するため、スクリプト自身には
トリガーを作成・削除する`script.scriptapp`スコープを与えていません。定期実行の
間隔は手動で選択し、同じ関数のトリガーを重複して作成しないでください。

### MFAが有効な場合

TP-Linkは、アカウント設定で2段階認証を有効にしていない場合でも、新しい端末IDの
初回ログインに確認を要求することがあります。`initializeTapoSession()`が端末確認を
要求された場合、対応メールアドレスへ確認コードを要求し、処理IDを安全に保存して
停止します。メールで届いたコードを`TAPO_MFA_CODE`というScript Propertyへ一時的に
設定し、`completeTapoMfa()`を実行してください。確認時には端末をアカウントへ登録する
ため、通常は次回以降コードを要求されません。コードは成功・失敗にかかわらず実行後に
自動削除されます。その後、`runCollection()`を実行してください。

## 履歴のバックフィル

`backfillEnergyHistory()`を手動実行すると、機器が対応している範囲で次を取得します。

- 当日: 1時間単位
- 当四半期: 1日単位
- 当年: 1か月単位
- 旧形式機器: 当月の日次および当年の月次

履歴バックフィルは毎回の定期トリガーには含めません。API負荷と重複サンプルを
避けるため、初期投入や欠損補完時だけ手動実行してください。機器ごとの保存期間や
対応解像度はファームウェアによって異なります。

## ローカル検証

本番APIへ接続しない単体テストだけを実行します。

```bash
npm test
npm run check
```

テストは署名、Thing API経路、単位変換、未知フィールド保持、OTLP JSON、
ホスト許可リスト、TLS例外の接続先制限、デバイス変更コマンドが存在しないことを
確認します。

## 参考資料

- [TP-Link Tapo Product Guide — Tapo Open API](https://static.tp-link.com/upload/soho/2024/202405/20240510/2024%20Tapo%20Product%20Guide_English.pdf)
- [python-kasa energy module](https://github.com/python-kasa/python-kasa/blob/master/kasa/smart/modules/energy.py)
- [TP-Link Cloud API protocol research](https://github.com/piekstra/tplink-cloud-api)
- [Tapo Cloud Thing API protocol research](https://github.com/stoyanov-x/tapo-things-api-hub-research/blob/main/docs/cloud-thing-api.md)
- [Grafana Cloud OTLP format considerations](https://grafana.com/docs/grafana-cloud/observe-and-act/send-data/otlp/otlp-format-considerations/)
- [OpenTelemetry OTLP specification](https://opentelemetry.io/docs/specs/otlp/)
- [Google Apps Script UrlFetchApp](https://developers.google.com/apps-script/reference/url-fetch/url-fetch-app)

本実装は上記のプロトコル情報を参考にした独立実装であり、参照リポジトリのソース
コードを同梱していません。
