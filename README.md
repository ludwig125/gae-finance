## 東証一部の全銘柄
- https://www.jpx.co.jp/markets/statistics-equities/misc/01.html

## 東京証券取引所の営業日・休日
- https://www.jpx.co.jp/corporate/calendar/index.html

## 日歩株価
| 銘柄        | 日付        | 始値                | 高値               | 安値             | 終値                                | 売買高                   | 修正後終値  |
|-------------|-------------|---------------------|--------------------|------------------|-------------------------------------|--------------------------|-------------|
|             |             | Opening price(open) | High price（high） | Low price（low） | Closing price（close）、 Last price | Turnover, Trading volume |             |
| code        | date        | open                | high               | low              | close                               | turnover                 | modified    |
| VARCHAR(10) | VARCHAR(10) | VARCHAR(15)         | VARCHAR(15)        | VARCHAR(15)      | VARCHAR(15)                         | VARCHAR(15)              | VARCHAR(15) |

```
CREATE TABLE daily (
	code VARCHAR(10) NOT NULL,
	date VARCHAR(10) NOT NULL,
	open VARCHAR(15),
	high VARCHAR(15),
	low VARCHAR(15),
	close VARCHAR(15),
	turnover VARCHAR(15),
	modified VARCHAR(15),
	PRIMARY KEY( code, date )
);
```
```
MySQL [stockprice]> show columns from daily;
+----------+-------------+------+-----+---------+-------+
| Field    | Type        | Null | Key | Default | Extra |
+----------+-------------+------+-----+---------+-------+
| code     | varchar(10) | NO   | PRI | NULL    |       |
| date     | varchar(10) | NO   | PRI | NULL    |       |
| open     | varchar(15) | YES  |     | NULL    |       |
| high     | varchar(15) | YES  |     | NULL    |       |
| low      | varchar(15) | YES  |     | NULL    |       |
| close    | varchar(15) | YES  |     | NULL    |       |
| turnover | varchar(15) | YES  |     | NULL    |       |
| modified | varchar(15) | YES  |     | NULL    |       |
+----------+-------------+------+-----+---------+-------+
8 rows in set (0.04 sec)
MySQL [stockprice]>
```

sample
```
mysql> select * from daily;
+------+------------+-------+------+------+-------+----------+----------+
| code | date       | open  | high | low  | close | turnover | modified |
+------+------------+-------+------+------+-------+----------+----------+
| 1301 | 2018/12/03 | 
3225 | 3335 | 3225 | 3320  | 43300    | 3320     |
+------+------------+-------+------+------+-------+----------+----------+
1 row in set (0.00 sec)

mysql> 
```
## 移動平均線
| 銘柄        | 日付        | 3日移動平均 | 5日移動平均 | 7日移動平均 | 10日移動平均 | 20日移動平均 | 60日移動平均 | 100日移動平均 |
|-------------|-------------|-------------|-------------|-------------|--------------|--------------|--------------|---------------|
| code        | date        | moving3     | moving5     | moving7     | moving10     | moving20     | moving60     | moving100     |
| VARCHAR(10) | VARCHAR(10) | DOUBLE      | DOUBLE      | DOUBLE      | DOUBLE       | DOUBLE       | DOUBLE       | DOUBLE        |

```
CREATE TABLE movingavg (
	code VARCHAR(10) NOT NULL,
	date VARCHAR(10) NOT NULL,
	moving3 DOUBLE,
	moving5 DOUBLE,
	moving7 DOUBLE,
	moving10 DOUBLE,
	moving20 DOUBLE,
	moving60 DOUBLE,
	moving100 DOUBLE,
	PRIMARY KEY( code, date )
);
```
```
MySQL [stockprice]>  show columns from movingavg;
+-----------+-------------+------+-----+---------+-------+
| Field     | Type        | Null | Key | Default | Extra |
+-----------+-------------+------+-----+---------+-------+
| code      | varchar(10) | NO   | PRI | NULL    |       |
| date      | varchar(10) | NO   | PRI | NULL    |       |
| moving3   | double      | YES  |     | NULL    |       |
| moving5   | double      | YES  |     | NULL    |       |
| moving7   | double      | YES  |     | NULL    |       |
| moving10  | double      | YES  |     | NULL    |       |
| moving20  | double      | YES  |     | NULL    |       |
| moving60  | double      | YES  |     | NULL    |       |
| moving100 | double      | YES  |     | NULL    |       |
+-----------+-------------+------+-----+---------+-------+
6 rows in set (0.04 sec)
MySQL [stockprice]>
```
