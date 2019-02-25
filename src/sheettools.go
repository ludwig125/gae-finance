package main

import (
	"fmt"
	"net/http"
	"time"

	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"
)

// spreadsheets clientを取得
func getSheetClient(r *http.Request) (*sheets.Service, error) {
	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)
	// spreadsheets clientを取得
	srv, err := sheets.New(client)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve Sheets Client %v", err)
	}
	return srv, nil
}

func getSheetData(r *http.Request, srv *sheets.Service, sheetId string, readRange string) [][]interface{} {
	ctx := appengine.NewContext(r)

	var MaxRetries = 3
	attempt := 0
	for {
		// MaxRetries を超えていたらnilを返す
		if attempt >= MaxRetries {
			log.Errorf(ctx, "Failed to retrieve data from sheet. attempt: %d. reached MaxRetries!", attempt)
			return nil
		}
		attempt = attempt + 1
		// stockpriceシートからデータを取得
		resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
		if err != nil {
			log.Warningf(ctx, "Unable to retrieve data from sheet: %v. attempt: %d", err, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		status := resp.ServerResponse.HTTPStatusCode
		if status != 200 {
			log.Warningf(ctx, "HTTPstatus error: %v. attempt: %d", status, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		return resp.Values
	}
}

func clearSheet(srv *sheets.Service, r *http.Request, sid string, sname string) {
	ctx := appengine.NewContext(r)

	// clear stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Clear(sid, sname, &sheets.ClearValuesRequest{}).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to clear value. %v", err)
		return
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
		return
	}
}

func writeRate(srv *sheets.Service, r *http.Request, rate []codeRate, sid string, sname string) {
	ctx := appengine.NewContext(r)

	// spreadsheetに書き込み対象の行列を作成
	matrix := make([][]interface{}, len(rate))
	// 株価の比率順にソートしたものを書き込み
	//for i, r := range rate {
	//matrix[i] = []interface{}{r.Code, r.Rate[0], r.Rate[1], r.Rate[2], r.Rate[3], r.Rate[4], r.Rate[5]}
	//}
	for _, r := range rate {
		m := make([]interface{}, 0)
		m = append(m, r.Code)
		// Rateの個数だけ書き込み
		for i := 0; i < len(r.Rate); i++ {
			m = append(m, r.Rate[i])
		}
		matrix = append(matrix, m)
	}

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values:         matrix,
	}
	// Write stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Append(sid, sname, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
		return
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
		return
	}
}
