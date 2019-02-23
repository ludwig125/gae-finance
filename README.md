東京証券取引所の営業日・休日
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
