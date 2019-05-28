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

func dialSQL(r *http.Request) (*sql.DB, error) {
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

// insert対象のtable名、項目名、レコードを引数に取ってDBに書き込む
func insertDB(r *http.Request, db *sql.DB, table string, columns []string, records [][]string) (int, error) {
	ctx := appengine.NewContext(r)

	// insert対象を組み立てる
	// TODO: +=の文字列結合は遅いので改良する
	ins := ""
	for _, record := range records {
		// 一行ごとに('項目1',..., '最後の項目'), の形でINSERT対象を組み立て
		ins += "("
		for i := 0; i < len(record)-1; i++ {
			ins += fmt.Sprintf("'%s',", record[i])
		}
		// 最後の項目だけ後ろに","が不要なので分けて記載
		ins += fmt.Sprintf("'%s'),", record[len(record)-1])
	}
	// 末尾の,を除去
	ins = strings.TrimRight(ins, ",")
	//log.Debugf(ctx, "ins: %v", ins)

	// 挿入対象の件数
	targetNum := len(records)

	log.Infof(ctx, "trying to insert %d values to '%s' table.", targetNum, table)
	// INSERT IGNORE INTO 'table名' (項目名1, 項目名2...) VALUES (...), (...)の形
	// queryを組み立て
	query := fmt.Sprintf("INSERT IGNORE INTO %s (", table)
	for _, c := range columns {
		query += fmt.Sprintf("%s,", c)
	}
	// 末尾の,を除去
	query = strings.TrimRight(query, ", ")
	query += fmt.Sprintf(") VALUES %s;", ins)

	//log.Debugf(ctx, "query: %v", query)
	rows, err := db.Query(query)
	if err != nil {
		log.Errorf(ctx, "failed to insert table: %s, err: %v, query: %v", table, err, query)
		return 0, err
	}
	defer rows.Close()
	return targetNum, nil
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

func selectTable(r *http.Request, db *sql.DB, q string) ([]string, error) {
	ctx := appengine.NewContext(r)
	log.Infof(ctx, "select query: %s", q)

	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("failed to select. query: [%s], err: %v", q, err)
	}
	defer rows.Close()

	// 参考：https://github.com/go-sql-driver/mysql/wiki/Examples
	// テーブルから列名を取得する
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %v", err)
	}

	// 列の長さ分だけのvalues
	// see https://golang.org/pkg/database/sql/#RawBytes
	// RawBytes is a byte slice that holds a reference to memory \
	// owned by the database itself.
	// After a Scan into a RawBytes, \
	// the slice is only valid until the next call to Next, Scan, or Close.
	values := make([]sql.RawBytes, len(columns))

	// rows.Scan は引数として'[]interface{}'が必要なので,
	// この引数scanArgsに列のサイズだけ確保した変数の参照をコピー
	// See http://code.google.com/p/go-wiki/wiki/InterfaceSlice for details
	scanArgs := make([]interface{}, len(values))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	// select結果を詰める入れ物
	retVals := make([]string, 0)

	// Fetch rows
	for rows.Next() {
		// get RawBytes from data
		err = rows.Scan(scanArgs...)
		if err != nil {
			return nil, fmt.Errorf("failed to scan: %v", err)
		}

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
		return nil, fmt.Errorf("row error: %v", err)
	}
	return retVals, nil
}
