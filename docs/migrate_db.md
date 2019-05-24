# 無料トライアル期間が終わったのでCloudSQLのマイグレーションをした話

## 事象
2019/5/22 CloudSQLに接続できなくなった

```
$ gcloud sql connect myfinance --user=root
ERROR: (gcloud.sql.connect) HTTPError 409: The instance or operation is not in an appropriate state to handle the request.
```

GoogleAppEngineのログには以下が出力されていた
```
failed to insert table: daily, err: driver: bad connection, query: 
```

## 原因
Google Cloud PlatformのUIの一番上を見ると無料トライアル期間が終わったとを通知していた

![無料トライアルは終了しました](https://user-images.githubusercontent.com/18366858/58228105-800e4380-7d68-11e9-817c-acfdf6b932ec.png)

上のUIの右の「アップグレード」ボタンを押すと以下のように表示された


![アカウントのアップグレード](https://user-images.githubusercontent.com/18366858/58228180-cc598380-7d68-11e9-9aa0-647f2500d45b.png)


これを承諾するまえに、そもそも無料トライアル期間中はいくらくらい使っていたのか確認してみた

## これからかかるはずの料金の確認

#### レポートを見てみる

レポートの見方
- [請求レポートによる費用傾向の表示  \|  Cloud Billing のドキュメント  |  Google Cloud](https://cloud.google.com/billing/docs/how-to/reports?_ga=2.67382033.-1623686309.1552089908)

「お支払い」の「レポート」を見てみた

![image](https://user-images.githubusercontent.com/18366858/58228254-1e020e00-7d69-11e9-90fd-7ff718fb5425.png)

レポートの、先月分をプロダクト単位で見てみる
- グループ条件を変更

SKU単位で見てみると使用しているサービスの内訳がわかる

![image](https://user-images.githubusercontent.com/18366858/58296603-50664680-7e0f-11e9-9c58-67d1345d3bc5.png)

| SKU                                                                          | プロダクト | SKU ID         | 使用              | クレジット適用前の費用 | プロモーション | 割引 | クレジット適用後の費用 |
|------------------------------------------------------------------------------|------------|----------------|-------------------|------------------------|----------------|------|------------------------|
| DB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount) | Cloud SQL  | 5356-0253-5FBD | 720 hour          | ¥6,996                 | ¥-6,996        | —    | ¥0                     |
| Storage PD SSD for DB in Japan                                               | Cloud SQL  | 9D66-0506-E274 | 10 gibibyte month | ¥244                   | ¥-244          | —    | ¥0                     |

特にこれ
- DB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount)
  - クレジット適用前の費用：¥6,996

・・・なにこの値段
#### SKUって？[単品管理 - Wikipedia](https://ja.wikipedia.org/wiki/%E5%8D%98%E5%93%81%E7%AE%A1%E7%90%86)

> SKUは最小管理単位 (Stock Keeping Unit) の略。

Google Cloud PlatformにはSKUの意味について説明がなかったのでたぶんこれのことだと思う

[毎月の請求について  \|  Cloud Billing のドキュメント  |  Google Cloud](https://cloud.google.com/billing/docs/how-to/read-invoice?hl=ja)
> SKU ID	サービスが使用するリソースの ID。SKU の詳細な一覧については、GCP SKU をご覧ください。

#### GCP SKU一覧
[SKUs  \|  Google Cloud](https://cloud.google.com/skus/?currency=USD)


この単位で料金を決めているらしい

#### この金額が正当なのか確認

上で表示したレポートのSKUの一覧を見ると、
DB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount)
に一番金がかかっているらしい
- ¥6,996
![image](https://user-images.githubusercontent.com/18366858/58231379-d338c400-7d71-11e9-921f-7f5579408836.png)

この「DB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount)」を上の公式の表「GCP SKU一覧」から探すとあった
![image](https://user-images.githubusercontent.com/18366858/58231898-15163a00-7d73-11e9-8e15-aefdd59a7c61.png)


DB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount)
-  0.088 USD per hour

この0.088を4月中フル稼働した場合（720時間）で、現在の円ドルレート 約110円を当てはめると、
0.088*720*110=6969.6 となって上とほぼ等しい

#### with 30% promotional discount
この「with 30% promotional discount」というのは、
「継続利用割引」というサービスらしい

[Google Compute Engine の料金  \|  Compute Engine ドキュメント  |  Google Cloud](https://cloud.google.com/compute/pricing#sustained_use)
> 継続利用割引は、使用する vCPU とメモリ量ごとに計算されます。Compute Engine では、1 か月の 25% を超える期間にわたり vCPU またはメモリ量がインスタンスで使用されると、そのリソースの使用が 1 秒延びるごとに割引が自動的に適用されます。 使用時間が増えるほど割引率は高くなり、1 か月ずっと稼働するインスタンスでは vCPU とメモリの費用の最大 30% の正味割引を受けることができます。

## CloudSQLの料金体系について確認する
[Cloud SQL の料金  \|  Cloud SQL ドキュメント  |  Google Cloud](https://cloud.google.com/sql/pricing?hl=ja)

自分が使っているのはCloudSQLのMysql 第 2 世代なのでそれを見る

> MySQL 第 2 世代の料金
第 2 世代の料金は、次の料金で構成されます。
インスタンスの料金
ストレージの料金
ネットワークの料金

#### mysql 2ndの料金

東京（asia-northeast1）を見ると、
| マシンタイプ | 仮想 CPU 数      | RAM（GB） | 最大ストレージ容量 | 最大接続数 | 料金（米ドル） | 継続利用価格（米ドル） |
|------------------|-----------|--------------------|------------|----------------|------------------------|-------|
| db-n1-standard-1 | 1         | 3.75               | 10,230 GB  | 4,000          | $0.13                  | $0.09 |
となっていた。


![image](https://user-images.githubusercontent.com/18366858/58232481-899da880-7d74-11e9-9197-dea199e8cac9.png)


これは上のDB standard Intel N1 1 VCPU running in Japan (with 30% promotional discount) の 0.088という数字と一致する

なるほど・・・高い

#### 各インスタンスの設定
[インスタンスの設定  \|  Cloud SQL ドキュメント  |  Google Cloud](https://cloud.google.com/sql/docs/instance-settings?hl=ja)
> Google Cloud SQL の第 1 世代および第 2 世代のインスタンスで使用できるすべての設定について

こちらに詳しく書いてある

#### どうして720時間（一月休みなく動かし続けるとこの時間）使っていることになっているのか？

これが一番疑問だった

自分のサービスはCronで一日せいぜい２時間くらいしか動いていないのにどうして休みなく稼働していることになっているのか？

調べてみると、以下の通り明示的にDBを停止させない限り、ずーっと稼働していて、その分料金もかかるらしい

##### Mysqlの第 2 世代の特徴

[第 2 世代機能  \|  Cloud SQL ドキュメント  |  Google Cloud](https://cloud.google.com/sql/docs/1st-2nd-gen-differences?hl=ja)

> 料金の違い
Cloud SQL 第 2 世代では、従量制の料金パッケージが提供されていません。インスタンスの料金は、マシンタイプによって決まります。分単位の課金と継続利用割引の導入により、Cloud SQL 第 2 世代では多くのワークロードで費用対効果を高めることができます。詳しくは、料金のページをご覧ください。

##### アクティベーションポリシー

[インスタンスの設定  \|  Cloud SQL ドキュメント  |  Google Cloud](https://cloud.google.com/sql/docs/instance-settings?hl=ja#activation-policy-2ndgen)

> アクティベーション ポリシー
第 2 世代インスタンスの場合、アクティベーション ポリシーはインスタンスを起動または停止するためにのみ使用されます。アクティベーション ポリシーを [常にオン] に設定するとインスタンスが起動し、[オフ] に設定するとインスタンスが停止します。

**つまり、オフにしない限り利用し続けている状態となるらしい**

- 第１世代にはON DEMAND というポリシーがあって、リクエストが来たら起動する（立ち上がりに時間かかるけど）という仕組みがあったらしいが、それは第２世代にはない

#### インスタンスの起動、停止はどうすればいいのか
[インスタンスの起動、停止、再起動  \|  Cloud SQL  |  Google Cloud](https://cloud.google.com/sql/docs/start-stop-restart-instance?hl=ja)

ブラウザまたはgcloudコマンドで起動・停止できるらしい

## 料金を安くできないか

以上より、料金を安く抑えてCloudSQLを使うためには以下の３つが考えられそう

1. インスタンスのリージョンを安いところにする
2. マシンを一番安いものにする
3. 使わないときはインスタンスを止める

3番目については自動でgcloudコマンドを実行できるようにそのうちしたい

#### 一番安いリージョンの一番安いマシンを調べる
https://cloud.google.com/sql/pricing?hl=ja

をいろいろ見た結果、
アメリカなどのリージョンのdb-f1-microマシンが一番安かったのでこれを選択
なんでも良かったので、アイオワにする

![image](https://user-images.githubusercontent.com/18366858/58232854-94a50880-7d75-11e9-8436-0afb3cdb7880.png)

| リージョン | マシンタイプ | 仮想 CPU 数      | RAM（GB） | 最大ストレージ容量 | 最大接続数 | 料金（米ドル） | 継続利用価格（米ドル） |
|--------------|------------------|-----------|--------------------|------------|----------------|------------------------|-------|
| アイオワ     | db-f1-micro*     | 共有      | 0.6                | 3,062 GB   | 250            | $0.0150	                  | $0.0105 |
| 東京         | db-n1-standard-1 | 1         | 3.75               | 10,230 GB  | 4,000          | $0.1255	                 | $0.0878 |

**アイオワのdb-f1-micro は東京のdb-n1-standard-1の１／８以下**

**継続利用価格（米ドル）では$0.0105 / hour**

※この料金は一時間あたりの利用料金のことらしい

[Google Cloud Platform 料金計算ツール  \|  Google Cloud Platform  |  Google Cloud](https://cloud.google.com/products/calculator/#id=844f770c-560c-4ea7-a04d-8ed799b2817c)
- このツールで見積もりもできる

自分のサービスはSLAがめちゃめちゃ低く、レイテンシーはほとんど気にしない。データサイズも数十GBなので、
db-f1-microで十分そう

想定費用を計算してみた

| リージョン×マシンタイプ | 料金（24時間×31日×110円 ※ 110は現在のレート）    |
|--------------|-----------|
| アイオワ db-f1-micro  | 0.0105*24*31*110=859.32円  |
| 東京 db-n1-standard-1o  | 0.0878*24*31*110＝7185.552円  |

インスタンスの他にストレージとネットワークの料金が別にかかるが、一旦これだけ考える

もとの金額に比べれば、趣味で使う金額としてだいぶ現実的になった

#### ストレージについて

ストレージはSSDのほうが当然高い

でもこれくらいなら許容しようかな（あとで変えられるのでSSDを選ぶ）
- $0.17 per GB/month for SSD storage capacity
- $0.09 per GB/month for HDD storage capacity

##### SKUの表で料金を確認
https://cloud.google.com/skus/?currency=JPY&filter=micro
![image](https://user-images.githubusercontent.com/18366858/58287556-43842b80-7dec-11e9-9499-1936542b9f42.png)

CloudSQLの表と名前が一対一対応していないのでわかりにくいけど、おそらくこれが対応するSKUとその金額だと思える

DB generic Micro instance with burstable CPU running in Americas (with 30% promotional discount)
- 0.0105 USD per hour

# 新規DBの作成

## アップグレード

最初のアップグレードボタンを押して使えるようにする

## インスタンスの作成

https://console.cloud.google.com/sql/instances
から、アイオワリージョンのdb-f1-microを新しく作る

![image](https://user-images.githubusercontent.com/18366858/58294783-dd58d200-7e06-11e9-884f-bc492cbec34b.png)
「インスタンスを作成」

![image](https://user-images.githubusercontent.com/18366858/58294803-f497bf80-7e06-11e9-917b-dda1b8862dbb.png)

Mysqlを選択

![image](https://user-images.githubusercontent.com/18366858/58294805-ff525480-7e06-11e9-8e95-90ae5158844c.png)

任意のインスタンスIDとパスワードを設定する

このときに下の「設定オプションを表示」を押すと、マシンタイプなどを細かく設定できる
- リージョンはあとから変えられない

**※あとでも変更できるけど、何も設定しないとdb-n1-standard-1になってしまうので超注意！！**

## インスタンスの編集
上の、「設定オプションを表示」をいじらなくても、あとからマシンタイプなどは変えられる

[Google Cloud Platform](https://console.cloud.google.com/sql/instances/myfinance/edit-performance-class?project=myfinance-01&walkthrough_tutorial_id=sql_connect_gce_vm)
のSQLでインスタンスが見られるので、その画面の中の「設定」→「設定を編集」から設定を変更できる

![image](https://user-images.githubusercontent.com/18366858/58294991-d1b9db00-7e07-11e9-9d45-cc0af77c336b.png)

#### マシンタイプとストレージの設定

マシンタイプとストレージの設定を編集

**変更前**

![マシンタイプとストレージ](https://user-images.githubusercontent.com/18366858/58297329-501b7a80-7e12-11e9-9ce5-e0691fde3c56.png)

**変更後**

![image](https://user-images.githubusercontent.com/18366858/58295120-8e13a100-7e08-11e9-810b-90d9d7562941.png)

- マシンタイプ：db-f1-micro
- ストレージの種類：SSD
- ストレージ容量 ：最低の10GB
- ストレージの自動増量を有効化：チェックした

#### 自動バックアップの有効化

しない
- とりあえず、自分で定期的に頑張ることにする

![image](https://user-images.githubusercontent.com/18366858/58295224-0a0de900-7e09-11e9-9b00-312f55852b6e.png)

# 新旧DBデータ移行汎用手順

#### 0. 準備

[Cloud SQL for MySQL の使用  \|  Go の App Engine スタンダード環境  |  Google Cloud](https://cloud.google.com/appengine/docs/standard/go/cloud-sql/using-cloud-sql-mysql?hl=ja)

[Cloud SQL for MySQL のクイックスタート  \|  Cloud SQL for MySQL  |  Google Cloud](https://cloud.google.com/sql/docs/mysql/quickstart)

これに従ってcloud_sql_proxyをローカルマシンに入れておく


事前に新旧DBのインスタンス接続名を調べておく

[Google Cloud Platform](https://console.cloud.google.com/home/dashboard?project=myfinance-01&folder=&organizationId=) 
の左上のボタンを押してからSQLを選んで、

![image](https://user-images.githubusercontent.com/18366858/58297120-8278a800-7e11-11e9-8301-60974e7d06d5.png)

対象のインスタンスIDを選択して、
「このインスタンスに接続」->「インスタンス接続名」から取得できる

![このインスタンスに接続](https://user-images.githubusercontent.com/18366858/58297222-f1ee9780-7e11-11e9-9df8-890cb8ed0f1c.png)


#### 1. 旧DBへのプロキシサーバ立ち上げ

ローカルの端末で以下を実施
```
$ cloud_sql_proxy -instances=<インスタンス接続名>=tcp:3307
```

#### 2. 旧DBのテーブルをDump（Export）


別の端末を開いてTableデータをDump
```
$mysqldump -u root  -p --host 127.0.0.1 --port 3307 <Database名> <テーブル名> > dumpdata.sql
```

- Database名: show databases; で出てくるなかの対象のDatabase

#### 3. 新DBのテーブルへImport

新DBへのプロキシサーバ立ち上げ

先程までの旧DBに繋がっていたプロキシサーバは止めて、ローカルの端末で以下を実施
```
$ cloud_sql_proxy -instances=<新DBのインスタンス接続名>=tcp:3307
```

#### 4. 新DBでDatabaseを作っておく

別の端末で新DBに接続
```
$mysql -u root -p --host 127.0.0.1 --port 3307


MySQL [(none)]> CREATE DATABASE <Database名>;
```

#### 5. 新DBのテーブルへImport


別の端末を開いてTableデータをImport
```
$mysql -u root -p --host 127.0.0.1 --port 3307 <Database名> < dumpdata.sql
```


# 新旧DBデータ移行(自分のDB)
## 旧DBのデータをExport

端末でプロキシサーバ立ち上げておく
```
$./cloud_sql_proxy -instances=myfinance-01:asia-northeast1:myfinance=tcp:3307

```
別の端末でDump
```
$mysqldump -u root  -p --host 127.0.0.1 --port 3307 stockprice daily > dumpdaily.sql
```

## 新DBで事前にDatabaseを作成しておく

端末で新DBへのプロキシサーバを立ち上げておく
```
$./cloud_sql_proxy -instances=myfinance-01:us-central1:myfinance-us-central1=tcp:3307
```

別の端末で新DBに接続
```
$mysql -u root -p --host 127.0.0.1 --port 3307
```

```
MySQL [(none)]> CREATE DATABASE stockprice;
Query OK, 1 row affected (0.17 sec)

MySQL [(none)]>
MySQL [(none)]> show databases;
+--------------------+
| Database           |
+--------------------+
| information_schema |
| mysql              |
| performance_schema |
| stockprice         |
| sys                |
+--------------------+
5 rows in set (0.16 sec)

MySQL [(none)]>
```

## 新DBにデータをImport

事前にDumpしておいたファイルをImport
```
[~/go/src/github.com/gae-finance/src] $mysql -u root -p --host 127.0.0.1 --port 3307 stockprice < dumpdaily.sql
Enter password: 
ERROR 1227 (42000) at line 18: Access denied; you need (at least one of) the SUPER privilege(s) for this operation
[~/go/src/github.com/gae-finance/src] $   
```


よくわからないし重要だと思えず、調べるのも面倒なのでdumpファイル中の以下の部分をコメントアウトした
```
SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;
SET @@SESSION.SQL_LOG_BIN= 0;

--
-- GTID state at the beginning of the backup 
--

SET @@GLOBAL.GTID_PURGED='f70265b8-1bf9-11e9-8d2e-42010a920fc8:1-10512405';

SET @@SESSION.SQL_LOG_BIN = @MYSQLDUMP_TEMP_LOG_BIN;

```

再度実行
```
[~/go/src/github.com/gae-finance/src] $mysql -u root -p --host 127.0.0.1 --port 3307 stockprice < dumpdaily.sql
Enter password:
[~/go/src/github.com/gae-finance/src] $
```

こんどはDumpに成功したっぽい
```
mysql> show tables;
+----------------------+
| Tables_in_stockprice |
+----------------------+
| daily                |
+----------------------+
1 row in set (0.14 sec)

mysql> 
mysql> select count(*) from daily;
+----------+
| count(*) |
+----------+
|   303177 |
+----------+
1 row in set (0.36 sec)

mysql> 
```

旧DBの件数
```
MySQL [stockprice]> select count(*) from daily;
+----------+
| count(*) |
+----------+
|   303377 |
+----------+
1 row in set (1.61 sec)

MySQL [stockprice]> select count(*) from daily where date = '2019/05/21';
+----------+
| count(*) |
+----------+
|      200 |
+----------+
1 row in set (0.83 sec)
```

新DBと旧DBの最新の日付のデータがない
```
新DB
mysql> select date from daily order by date desc limit 1;
+------------+
| date       |
+------------+
| 2019/05/20 |
+------------+
1 row in set (0.39 sec)

mysql> 

旧DB
MySQL [stockprice]> select date from daily order by date desc limit 1;
+------------+
| date       |
+------------+
| 2019/05/21 |
+------------+
1 row in set (0.97 sec)

MySQL [stockprice]>
```

# 新旧DBのパフォーマンス比較

あとで

そんなにひどくはならなかった



