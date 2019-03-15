package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"

	_ "github.com/go-sql-driver/mysql"
)

func dialSql(r *http.Request) (*sql.DB, error) {
	ctx := appengine.NewContext(r)

	p := ""
	if !appengine.IsDevAppServer() {
		// prod環境ならPASSWORD必須
		log.Infof(ctx, "this is prod. trying to fetch CLOUDSQL_PASSWORD")
		p = mustGetenv(r, "CLOUDSQL_PASSWORD")
	}
	var (
		user           = mustGetenv(r, "CLOUDSQL_USER")
		password       = p
		connectionName = mustGetenv(r, "CLOUDSQL_CONNECTION_NAME")
	)

	if appengine.IsDevAppServer() {
		// DB名を指定しない時は以下のように/のみにする
		//return sql.Open("mysql", "root@/")
		return sql.Open("mysql", "root@/stockprice")
	}
	return sql.Open("mysql", fmt.Sprintf("%s:%s@cloudsql(%s)/stockprice", user, password, connectionName))
}

func insertDailyPrice(r *http.Request, db *sql.DB, table string, resp [][]interface{}) {
	ctx := appengine.NewContext(r)

	// insert対象を組み立てる
	// TODO: +=の文字列結合は遅いので改良する
	// TODO: 項目指定しなくてすむように汎用化する https://qiita.com/hironobu_s/items/6af7dd739b7aa9453dd5
	ins := ""
	for _, v := range resp {
		//log.Infof(ctx, "%v", v)
		ins += fmt.Sprintf("('%s', '%s', '%s', '%s', '%s', '%s', '%s', '%s'),", v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7])
	}
	// 末尾の,を除去
	ins = strings.TrimRight(ins, ",")

	log.Infof(ctx, "trying to insert %d dailyprice", len(resp))
	// INSERT IGNORE INTO daily (code, date, open, high, low, close, turnover, modified) VALUES (...), (),
	query := fmt.Sprintf("INSERT IGNORE INTO daily (code, date, open, high, low, close, turnover, modified) VALUES %s;", ins)
	log.Debugf(ctx, "query: %v", query)
	rows, err := db.Query(query)
	if err != nil {
		log.Errorf(ctx, "failed to insert table: %s, err: %v, query: %v", table, err, query)
		return
	}
	log.Infof(ctx, "succeded to insert dailyprice")
	defer rows.Close()
}

func showDatabases(w http.ResponseWriter, db *sql.DB) {
	w.Header().Set("Content-Type", "text/plain")

	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		// ここあとでGAE用のログに変える
		http.Error(w, fmt.Sprintf("Could not query db: %v", err), 500)
		return
	}
	defer rows.Close()

	buf := bytes.NewBufferString("Databases:\n")
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			http.Error(w, fmt.Sprintf("Could not scan result: %v", err), 500)
			return
		}
		fmt.Fprintf(buf, "- %s\n", dbName)
	}
	w.Write(buf.Bytes())
}

func selectTable(r *http.Request, db *sql.DB, q string) []interface{} {
	ctx := appengine.NewContext(r)

	rows, err := db.Query(q)
	if err != nil {
		log.Errorf(ctx, "failed to select. query: [%s], err: %v", q, err)
		return nil
	}
	defer rows.Close()

	// 参考：https://github.com/go-sql-driver/mysql/wiki/Examples
	// テーブルから列名を取得する
	columns, err := rows.Columns()
	if err != nil {
		log.Errorf(ctx, fmt.Sprintf("failed to get columns: %v", err))
	}

	// rows.Scan は引数として'[]interface{}'が必要なので,
	// この引数scanArgsに列のサイズだけ確保した変数の参照をコピー
	// See http://code.google.com/p/go-wiki/wiki/InterfaceSlice for details
	values := make([]sql.RawBytes, len(columns))
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	// select結果を詰める入れ物
	retVals := make([]interface{}, 0)

	// Fetch rows
	for rows.Next() {
		// get RawBytes from data
		err = rows.Scan(scanArgs...)
		if err != nil {
			log.Errorf(ctx, "failed to scan: %v", err)
		}

		// Now do something with the data.
		// Here we just print each column as a string.
		for _, col := range values {
			// Here we can check if the value is nil (NULL value)
			if col == nil {
				retVals = append(retVals, "")
			} else {
				retVals = append(retVals, string(col))
			}
		}
	}
	if err = rows.Err(); err != nil {
		log.Errorf(ctx, "row error: %v", err)
	}
	return retVals
}
