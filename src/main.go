package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch" // 外部にhttpするため
)

// codeごとの株価 スクレイピングした時のcode毎のデータがPriceに入る
type codePrice struct {
	Code  string
	Price []string
}

// codeごとの株価比率
type codeRate struct {
	Code string
	Rate []float64
}

// 日付と終値
type dateClose struct {
	Date  string
	Close int64
}

func main() {
	http.HandleFunc("/_ah/start", start)
	http.HandleFunc("/daily", indexHandlerDaily)
	//http.HandleFunc("/calc_daily", indexHandlerCalcDailyOld)
	http.HandleFunc("/movingavg", movingAvgHandler)
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/daily_to_sql", dailyToSqlHandler)
	http.HandleFunc("/delete_sheet", deleteSheetHandler)
	appengine.Main() // Starts the server to receive requests
}

// バッチ処理のbasic_scalingを使うために /_ah/startのハンドラが必要
func start(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	log.Infof(c, "STARTING")
}

func dailyToSqlHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// 環境変数を最初に読み込み
	getEnv(r)

	log.Infof(ctx, "appengine.IsDevAppServer: %v", appengine.IsDevAppServer())
	// 色々したあとに環境変数の読み込みに失敗するのは嫌なのでここで取得しておく
	MAX_SQL_INSERT, err := strconv.Atoi(mustGetenv(r, "MAX_SQL_INSERT"))
	if err != nil {
		log.Errorf(ctx, "failed to get MAX_SQL_INSERT. err: %v", err)
		os.Exit(0)
	}

	// cloud sql(ローカルの場合はmysql)と接続
	db, err := dialSql(r)
	if err != nil {
		log.Errorf(ctx, "Could not open db: %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "Succeded to open db")

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
		os.Exit(0)
	}
	// spreadsheetからdailypriceを取得
	resp := getSheetData(r, sheetService, DAILYPRICE_SHEETID, "daily")
	if resp == nil {
		log.Errorf(ctx, "failed to fetch sheetdata: '%v'", resp)
		os.Exit(0)
	}

	// MAX_SQL_INSERT件数ごとにsqlに書き込む
	length := len(resp)
	for begin := 0; begin < length; begin += MAX_SQL_INSERT {
		// 最初に書き込むレコードは 0〜MAX_SQL_INSERT-1
		// 次に書き込むレコードは MAX_SQL_INSERT〜 MAX_SQL_INSERT+MAX_SQL_INSERT-1
		end := begin + MAX_SQL_INSERT

		// endがデータ全体の長さを上回る場合は調節
		if end >= length {
			end = length
		}

		// dailypriceをcloudsqlに挿入
		insertDailyPrice(r, db, "daily", resp[begin:end])
	}
}

// 本日分のデータがスプレッドシートとDB両方に書き込まれていたらスプレッドシートのデータを消す
func deleteSheetHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// 環境変数を最初に読み込み
	getEnv(r)

	log.Infof(ctx, "delete daily sheet data")

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
		os.Exit(0)
	}

	// 休みの日だったら起動しない
	if !isBussinessday(sheetService, r) {
		log.Infof(ctx, "Is not a business day today.")
		return
	}

	// 休日データを取得
	holidays := getHolidays(r, sheetService)
	holidayMap := map[string]bool{}
	for _, row := range holidays {
		holiday := row[0].(string)
		holidayMap[holiday] = true
	}

	// test環境のスプレッドシートデータの最新の日付に合わせる
	previousBussinessDay := "2018/09/18"
	// prod環境の場合は、直近の取引日を取得する
	// 一日前から順番に見ていって、直近の休日ではない日を取引日として設定する
	if ENV != "test" {
		jst, _ := time.LoadLocation("Asia/Tokyo")
		now := time.Now().In(jst)
		// 直近の営業日を取得
		getPreviousBussinessDay := func() string {
			// 無限ループは嫌なので直近30日間見て取引日が見つからなかったら""を返して失敗
			for i := 1; i <= 30; i++ {
				previous := now.AddDate(0, 0, -i).Format("2006/01/02")
				if !holidayMap[previous] {
					return previous
				}
			}
			return ""
		}
		previousBussinessDay = getPreviousBussinessDay()
		if previousBussinessDay == "" {
			log.Errorf(ctx, "there are no previous bussinessdays")
			os.Exit(0)
		}
		log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)
	}

	// あとで全銘柄と比較するためにsheetの直近の取引日のデータに含まれる銘柄を取得してmapに格納
	codesInSheet := func() map[int]bool {
		// spreadsheetからdailypriceを取得
		sheetRet := getSheetData(r, sheetService, DAILYPRICE_SHEETID, "daily")
		if sheetRet == nil {
			log.Errorf(ctx, "failed to fetch sheetdata. err: '%v'.", sheetRet)
			os.Exit(0)
		}
		log.Infof(ctx, "number of sheet data %d", len(sheetRet))

		sheetCodesMap := map[int]bool{}
		log.Infof(ctx, "fetch code from spreadsheet. target date: '%s'", previousBussinessDay)
		for _, v := range sheetRet {
			// 銘柄、日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値の配列から
			// 日付を抜き出して、previousBussinessDayと一致したらその銘柄ID(code)を取得
			if v[1] == previousBussinessDay {
				code, _ := strconv.Atoi(v[0].(string)) // int型に変換
				sheetCodesMap[code] = true
			}
		}
		log.Infof(ctx, "sheetcodes %v", sheetCodesMap)
		return sheetCodesMap
	}
	sheetCodesMap := codesInSheet()

	// あとで全銘柄と比較するためにDBの直近の取引日のデータに含まれる銘柄を取得してmapに格納
	codesInDb := func() map[int]bool {
		// cloud sql(ローカルの場合はmysql)と接続
		db, err := dialSql(r)
		if err != nil {
			log.Errorf(ctx, "Could not open db: %v", err)
			os.Exit(0)
		}
		log.Infof(ctx, "Succeded to open db")

		query := fmt.Sprintf("SELECT code FROM daily WHERE date = '%s'", previousBussinessDay)
		dbRet := fetchSelectResult(r, db, query)
		//		log.Infof(ctx, "select query: %s", query)
		//		dbRet := selectTable(r, db, query)
		//		if dbRet == nil {
		//			log.Errorf(ctx, "selectTable failed")
		//			os.Exit(0)
		//		}

		// あとで全銘柄と比較するためにmapに格納
		dbCodesMap := map[int]bool{}
		for _, v := range dbRet {
			code, _ := strconv.Atoi(v.(string)) // int型に変換
			dbCodesMap[code] = true
		}
		log.Infof(ctx, "dbcodes %v", dbCodesMap)
		return dbCodesMap
	}
	dbCodesMap := codesInDb()

	// spreadsheetから銘柄コードを取得
	codes := getSheetData(r, sheetService, CODE_SHEETID, "ichibu")
	if codes == nil || len(codes) == 0 {
		log.Errorf(ctx, "failed to fetch sheetdata. err: '%v'.", codes)
		os.Exit(0)
	}
	// 全銘柄分がsheetにあるか確認する
	var notExistInSheet []int
	var notExistInDb []int
	for _, v := range codes {
		code, _ := strconv.Atoi(v[0].(string))
		if !sheetCodesMap[code] {
			// sheetになければnotExistInSheetにその銘柄を追加
			notExistInSheet = append(notExistInSheet, code)
		}
		if !dbCodesMap[code] {
			// dbになければnotExistInDbにその銘柄を追加
			notExistInDb = append(notExistInDb, code)
		}
	}

	// sheetとdbを比較
	var notExistInDbExistInSheet []int
	// code->boolのうちcodeを取り出す
	for k, _ := range sheetCodesMap {
		if !dbCodesMap[k] {
			notExistInDbExistInSheet = append(notExistInDbExistInSheet, k)
		}
	}
	f := func(from string, to string, list []int) int {
		if len(list) != 0 {
			log.Errorf(ctx, "failed to write all '%s' data to '%s'. unmatched!! not exist in '%s': %v", from, to, to, list)
			return 1
		}
		log.Infof(ctx, "succeeded to write all '%s' data to '%s'.", from, to)
		return 0
	}
	// 全銘柄のうちsheetに書き込まれていない銘柄一覧と、sheetのうちDBに書き込まれていない銘柄一覧を列挙する
	// 全銘柄のうちDBに書き込まれていない銘柄一覧は見ない
	// どれか一つでも駄目だったら失敗
	// boolにすると、最初の一つが駄目だとそれ以降が判定されなくなったので、結果の和にした
	existFailues := f("codes", "sheet", notExistInSheet) + f("sheet", "db", notExistInDbExistInSheet)
	if existFailues != 0 {
		log.Errorf(ctx, "failed to write all data.")
		os.Exit(0)
	}

	// TODO: このあと実際にシートの中身を空にする処理を追加する

}

func mustGetenv(r *http.Request, k string) string {
	ctx := appengine.NewContext(r)
	v := os.Getenv(k)
	if v == "" {
		log.Errorf(ctx, "%s environment variable not set.", k)
		os.Exit(0)
	}
	log.Infof(ctx, "%s environment variable set.", k)
	return v
}

func indexHandlerDaily(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// read environment values
	getEnv(r)

	// 100件ずつ(test環境は10件)スクレイピングしてSheetに書き込み
	// 最初に環境変数を読み込む
	MAX_SHEET_INSERT, err := strconv.Atoi(mustGetenv(r, "MAX_SHEET_INSERT"))
	if err != nil {
		log.Errorf(ctx, "failed to get MAX_SHEET_INSERT. err: %v", err)
		os.Exit(0)
	}

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
		os.Exit(0)
	}

	if !isBussinessday(sheetService, r) {
		log.Infof(ctx, "Is not a business day today.")
		return
	}

	// spreadsheetから銘柄コードを取得
	//codes := readCode(sheetService, r, "ichibu")
	codes := getSheetData(r, sheetService, CODE_SHEETID, "ichibu")
	if codes == nil || len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		os.Exit(0)
	}

	// 重複書き込みをしないように既存のデータに目印をつける
	resp := getSheetData(r, sheetService, DAILYPRICE_SHEETID, "daily")
	if resp == nil {
		log.Errorf(ctx, "failed to fetch sheetdata: '%v'", resp)
		os.Exit(0)
	}
	existData := map[string]bool{}
	for _, v := range resp {
		// codeと日付の組をmapに登録
		cd := fmt.Sprintf("%s %s", v[0], v[1])
		existData[cd] = true
	}

	// 書き込み対象の件数
	target := 0
	// 書き込めた件数
	inserted := 0

	length := len(codes)
	for begin := 0; begin < length; begin += MAX_SHEET_INSERT {
		end := begin + MAX_SHEET_INSERT
		if end >= length {
			end = length
		}
		partial := codes[begin:end]
		tar, ins := scrapeAndWrite(r, partial, sheetService, existData)
		target += tar
		inserted += ins
	}

	log.Infof(ctx, "wrote records done. target: %d, inserted: %d", target, inserted)
	if target != inserted {
		log.Errorf(ctx, "failed to write all records. target: %d, inserted: %d", target, inserted)
		os.Exit(0)
	}
	log.Infof(ctx, "succeded to write all records.")
}

func scrapeAndWrite(r *http.Request, codes [][]interface{}, s *sheets.Service, existData map[string]bool) (int, int) {
	// MAX単位でcodeをScrapeしてSpreadSheetに書き込み
	prices := getEachCodesPrices(r, codes)
	// シートに存在しないものだけを抽出
	uniqPrices := getUniqPrice(r, prices, existData)

	target := 0
	inserted := 0
	// すでにシートに存在するデータは書き込まない
	if uniqPrices != nil {
		target, inserted = writeStockpriceDaily(r, s, uniqPrices)
	}
	return target, inserted
}

// 複数銘柄についてそれぞれの株価を取得する
func getEachCodesPrices(r *http.Request, codes [][]interface{}) []codePrice {
	ctx := appengine.NewContext(r)

	var prices []codePrice

	var allErrors string
	for _, v := range codes {
		code := v[0].(string) // row's type: []interface {}. ex. [8411]

		// codeごとに株価を取得
		// pは一ヶ月分の株価
		p, err := doScrapeDaily(r, code)
		if err != nil {
			//log.Infof(ctx, "err: %v", err)
			allErrors += fmt.Sprintf("%v ", err)
			continue
		}
		// code, 株価の単位でpricesに格納
		for _, dp := range p {
			prices = append(prices, codePrice{code, dp})
		}
		time.Sleep(1 * time.Second) // 1秒待つ
	}
	if allErrors != "" {
		// 複数の銘柄で起きたエラーをまとめて出力
		log.Warningf(ctx, "failed to scrape. code: [%s]\n", allErrors)
	}
	return prices
}

func movingAvgHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// TODO: 休日には動かないようにあとでする
	//	// read environment values
	//	getEnv(r)
	//	// spreadsheetのclientを取得
	//	sheetService, err := getSheetClient(r)
	//	if err != nil {
	//		log.Errorf(ctx, "err: %v", err)
	//		os.Exit(0)
	//	}
	//	if !isBussinessday(sheetService, r) {
	//		log.Infof(ctx, "Is not a business day today.")
	//		return
	//	}

	// cloud sql(ローカルの場合はmysql)と接続
	db, err := dialSql(r)
	if err != nil {
		log.Errorf(ctx, "Could not open db: %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "Succeded to open db")

	// 最新の日付にある銘柄を取得
	codes := fetchSelectResult(r, db,
		"SELECT code FROM daily WHERE date = (SELECT date FROM daily ORDER BY date DESC LIMIT 1);")

	// codeと件数(指定しない場合は0)を与えると、日付と終値の構造体を直近の日付順にして配列で返す関数
	orderedDateClose := func(code string, limit int) []dateClose {
		limitStr := ""
		if limit != 0 {
			limitStr = fmt.Sprintf("LIMIT %d", limit)
		}

		dbRet := fetchSelectResult(r, db, fmt.Sprintf(
			"SELECT date, close FROM daily WHERE code = %s ORDER BY date DESC %s;", code, limitStr))

		var dateCloses []dateClose
		// 日付と終値の２つを取得
		for i := 0; i < len(dbRet); i += 2 {
			// int32型数値に変換
			//c, _ := strconv.Atoi(dbRet[i+1].(string))
			c, _ := strconv.ParseInt(dbRet[i+1].(string), 10, 64)
			dateCloses = append(dateCloses, dateClose{Date: dbRet[i].(string), Close: c})
			//log.Infof(ctx, "%s %s", dbRet[i], dbRet[i+1])
		}
		return dateCloses
	}

	//	calcCloseRate := func(dcs []dateClose) {
	//		for i := 0; i < len(dcs)-1; i++ {
	//			// 前日との比率
	//			log.Infof(ctx, "date %s close %d rate %f", dcs[i].Date, dcs[i].Close, float64(dcs[i].Close)/float64(dcs[i+1].Close))
	//		}
	//	}

	var wg sync.WaitGroup
	wg.Add(len(codes))
	for _, code := range codes {
		go func(code string) {
			defer wg.Done()
			// 直近 100日分新しい順にソートして取得
			dcs := orderedDateClose(code, 100)

			// DBから取得できた日付のリスト
			var dateList []string
			for date := 0; date < len(dcs); date++ {
				dateList = append(dateList, dcs[date].Date)
			}

			// 取得対象の移動平均
			mvAvgList := []int{5, 20, 60, 100}
			// (日付;移動平均)のMapを5, 20,...ごとに格納したMap
			daysDateMovingMap := make(map[int]map[string]float64)
			for _, d := range mvAvgList {
				// 移動平均の計算
				daysDateMovingMap[d] = movingAverage(dcs, d)
			}
			// 移動平均をDBに書き込み
			insertMovingAvg(r, db, "movingavg", code, dateList, daysDateMovingMap)

		}(code.(string)) // codeはinterface型なのでキャストする
	}
	wg.Wait()
	//	for _, code := range codes {
	//		// 直近 100日分最近から順にソートして取得
	//		dcs := orderedDateClose(code.(string), 100)
	//
	//		// DBから取得できた日付のリスト
	//		var dateList []string
	//		for date := 0; date < len(dcs); date++ {
	//			dateList = append(dateList, dcs[date].Date)
	//		}
	//
	//		// 取得対象の移動平均
	//		mvAvgList := []int{5, 20, 60, 100}
	//		// (日付;移動平均)のMapを5, 20,...ごとに格納したMap
	//		daysDateMovingMap := make(map[int]map[string]float64)
	//		for _, d := range mvAvgList {
	//			daysDateMovingMap[d] = movingAverage(dcs, d)
	//		}
	//		// 移動平均をDBに書き込み
	//		insertMovingAvg(r, db, "movingavg", code.(string), dateList, daysDateMovingMap)
	//
	//	}
}

// X日移動平均線を計算する
//func movingAverage(r *http.Request, dcs []dateClose, avgDays int) map[string]float64 {
//	// GAE log
//	ctx := appengine.NewContext(r)
func movingAverage(dcs []dateClose, avgDays int) map[string]float64 {

	dateMovingMap := make(map[string]float64) // 日付と移動平均のMap

	// 与えられた日付-終値の要素数
	length := len(dcs)
	for date := 0; date < length; date++ {

		// X日移動平均のX(avgDays)を定義
		days := avgDays
		// Xが残りのデータ数より多かったら残りのデータ数がdaysになる
		if date+days > length {
			days = length - date
		}
		var sum int64
		for i := date; i < date+days; i++ {
			sum += dcs[i].Close
		}

		movingAvg := float64(sum) / float64(days)
		dateMovingMap[dcs[date].Date] = movingAvg

		//	log.Infof(ctx, "%d average %s %d %f", avgDays, dcs[date].Date, dcs[date].Close, movingAvg)
	}

	//return movingAvgList
	//log.Infof(ctx, "%d %v", avgDays, dateMovingMap)
	return dateMovingMap
}

//func indexHandlerCalcDailyOld(w http.ResponseWriter, r *http.Request) {
//	// GAE log
//	ctx := appengine.NewContext(r)
//
//	// read environment values
//	getEnv(r)
//
//	// spreadsheetのclientを取得
//	sheetService, err := getSheetClient(r)
//	if err != nil {
//		log.Errorf(ctx, "err: %v", err)
//		os.Exit(0)
//	}
//
//	if !isBussinessday(sheetService, r) {
//		log.Infof(ctx, "Is not a business day today.")
//		return
//	}
//
//	// spreadsheetから銘柄コードを取得
//	//codes := readCode(sheetService, r, "ichibu")
//	codes := getSheetData(r, sheetService, CODE_SHEETID, "ichibu")
//	if codes == nil || len(codes) == 0 {
//		log.Infof(ctx, "No target data.")
//		return
//	}
//
//	// spreadsheetから株価を取得する
//	resp := getSheetData(r, sheetService, DAILYPRICE_SHEETID, "daily")
//	if resp == nil {
//		log.Infof(ctx, "No data")
//		return
//	}
//
//	cdmp := codeDateModprice(r, resp)
//	//log.Infof(ctx, "%v\n", cdmp)
//
//	// 全codeの株価比率
//	var whole_codeRate []codeRate
//	for _, row := range codes {
//		code := row[0].(string)
//		//直近7日間の増減率を取得する
//		rate, err := calcIncreaseRate(cdmp, code, 7, r)
//		if err != nil {
//			log.Warningf(ctx, "%v\n", err)
//			continue
//		}
//		whole_codeRate = append(whole_codeRate, codeRate{code, rate})
//	}
//	log.Infof(ctx, "count whole code %v\n", len(whole_codeRate))
//
//	// 一つ前との比率が一番大きいもの順にソート
//	sort.SliceStable(whole_codeRate, func(i, j int) bool { return whole_codeRate[i].Rate[0] > whole_codeRate[j].Rate[0] })
//	//fmt.Fprintln(w, whole_codeRate)
//
//	// 事前にrateのシートをclear
//	clearSheet(sheetService, r, DAILYRATE_SHEETID, "daily_rate")
//
//	// 株価の比率順にソートしたものを書き込み
//	writeRate(sheetService, r, whole_codeRate, DAILYRATE_SHEETID, "daily_rate")
//}

func codeDateModprice(r *http.Request, resp [][]interface{}) [][]interface{} {
	//ctx := appengine.NewContext(r)
	matrix := make([][]interface{}, 0)
	for _, v := range resp {
		//log.Infof(ctx, "%v %v %v\n", v[0], v[1], v[len(v)-1])
		cdmp := []interface{}{
			interface{}(v[0]),        // 銘柄
			interface{}(v[1]),        // 日付
			interface{}(v[len(v)-1]), // 調整後終値
		}
		matrix = append(matrix, cdmp)
	}
	//log.Infof(ctx, "%v\n", matrix)
	return matrix
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// GAE log
	ctx := appengine.NewContext(r)

	// read environment values
	getEnv(r)

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
		os.Exit(0)
	}

	if !isBussinessday(sheetService, r) {
		log.Infof(ctx, "Is not a business day today.")
		return
	}

	// spreadsheetから銘柄コードを取得
	//codes := readCode(sheetService, r, "code")
	codes := getSheetData(r, sheetService, CODE_SHEETID, "code")
	if codes == nil || len(codes) == 0 {
		//if len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		os.Exit(0)
	}
	for _, row := range codes {
		code := row[0].(string)
		// codeごとに株価を取得
		date, stockprice, err := doScrape(r, code)
		if err != nil {
			log.Warningf(ctx, "Failed to scrape hourly price. stockcode: %s, err: %v\n", code, err)
			continue
		}

		fmt.Fprintln(w, code, date, stockprice)

		// 株価をspreadsheetに書き込み
		writeStockprice(sheetService, r, code, date, stockprice)

		time.Sleep(1 * time.Second) // 1秒待つ
	}
	// spreadsheetから株価を取得する
	resp := getSheetData(r, sheetService, STOCKPRICE_SHEETID, "stockprice")
	if resp == nil {
		log.Infof(ctx, "No data")
		return
	}

	// 全codeの株価比率
	var whole_codeRate []codeRate
	for _, row := range codes {
		code := row[0].(string)
		//直近7時間の増減率を取得する
		rate, err := calcIncreaseRate(resp, code, 7, r)
		if err != nil {
			log.Warningf(ctx, "%v\n", err)
			continue
		}
		whole_codeRate = append(whole_codeRate, codeRate{code, rate})
	}
	log.Infof(ctx, "count whole code %v\n", len(whole_codeRate))

	// 一つ前との比率が一番大きいもの順にソート
	sort.SliceStable(whole_codeRate, func(i, j int) bool { return whole_codeRate[i].Rate[0] > whole_codeRate[j].Rate[0] })
	fmt.Fprintln(w, whole_codeRate)

	// 事前にrateのシートをclear
	clearSheet(sheetService, r, RATE_SHEETID, "rate")

	// 株価の比率順にソートしたものを書き込み
	writeRate(sheetService, r, whole_codeRate, RATE_SHEETID, "rate")
}

var (
	CODE_SHEETID       string
	DAILYPRICE_SHEETID string
	ENV                string
	HOLIDAY_SHEETID    string
	STOCKPRICE_SHEETID string
	DAILYRATE_SHEETID  string
	RATE_SHEETID       string
)

func getEnv(r *http.Request) {
	ctx := appengine.NewContext(r)
	// 環境変数から読み込む

	CODE_SHEETID = mustGetenv(r, "CODE_SHEETID")
	DAILYPRICE_SHEETID = mustGetenv(r, "DAILYPRICE_SHEETID")
	ENV = mustGetenv(r, "ENV")
	if ENV != "test" && ENV != "prod" {
		// ENVがprodでもtestでもない場合は異常終了
		log.Errorf(ctx, "ENV must be 'test' or 'prod': %v", ENV)
		os.Exit(0)
	}
	HOLIDAY_SHEETID = mustGetenv(r, "HOLIDAY_SHEETID")
	STOCKPRICE_SHEETID = mustGetenv(r, "STOCKPRICE_SHEETID")
	DAILYRATE_SHEETID = mustGetenv(r, "DAILYRATE_SHEETID")
	RATE_SHEETID = mustGetenv(r, "RATE_SHEETID")

}

func isBussinessday(srv *sheets.Service, r *http.Request) bool {
	ctx := appengine.NewContext(r)

	if ENV == "test" {
		// test環境は常にtrue
		return true
	}

	// 以下はprodの場合

	// 土日は実行しない
	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)

	d := now.Weekday()
	switch d {
	case 6, 0: // Saturday, Sunday
		return false
	}

	today_ymd := now.Format("2006/01/02")

	//	// 'holiday' sheet を読み取り
	//	// 東京証券取引所の休日: https://www.jpx.co.jp/corporate/calendar/index.html
	//	holidays := getSheetData(r, srv, HOLIDAY_SHEETID, "holiday")
	//	if holidays == nil || len(holidays) == 0 {
	//		log.Infof(ctx, "Failed to get holidays.")
	//		os.Exit(0)
	//	}
	holidays := getHolidays(r, srv)
	for _, row := range holidays {
		holiday := row[0].(string)
		if holiday == today_ymd {
			log.Infof(ctx, "Today is holiday.")
			return false
		}
	}
	return true

}

func getHolidays(r *http.Request, srv *sheets.Service) [][]interface{} {
	ctx := appengine.NewContext(r)
	// 'holiday' sheet を読み取り
	// 東京証券取引所の休日: https://www.jpx.co.jp/corporate/calendar/index.html
	holidays := getSheetData(r, srv, HOLIDAY_SHEETID, "holiday")
	if holidays == nil || len(holidays) == 0 {
		log.Infof(ctx, "Failed to get holidays.")
		os.Exit(0)
	}
	return holidays
}

func doScrapeDaily(r *http.Request, code string) ([][]string, error) {
	// "DAILY_PRICE_URL"のHDML doc取得
	doc, err := fetchWebpageDoc(r, "DAILY_PRICE_URL", code)
	if err != nil {
		return nil, err
	}

	// date と priceを取得
	var date_price [][]string
	doc.Find(".m-tableType01_table table tbody tr").Each(func(i int, s *goquery.Selection) {
		date := s.Find(".a-taC").Text()
		re := regexp.MustCompile(`[0-9]+/[0-9]+`).Copy()
		// 日付を取得
		date = re.FindString(date)
		// 日付に年をつけたりゼロ埋めしたりする
		date = formatDate(date)

		var arr []string
		arr = append(arr, date)
		// 始値, 高値, 安値, 終値, 売買高, 修正後終値を順に取得
		s.Find(".a-taR").Each(func(j int, s2 *goquery.Selection) {
			// ","を取り除く
			arr = append(arr, strings.Replace(s2.Text(), ",", "", -1))
		})
		// 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値を一行ごとに格納
		date_price = append(date_price, arr)
	})
	if len(date_price) == 0 {
		return nil, fmt.Errorf("%s no data", code)
	}
	if len(date_price[0]) != 7 {
		// 以下の７要素を取れなかったら何かおかしい
		// 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値
		// リダイレクトされて別のページに飛ばされている可能性もある
		// 失敗した銘柄を返す
		return nil, fmt.Errorf("%s doesn't have enough elems", code)
	}
	return date_price, nil
}

func formatDate(date string) string {
	// 日付に年を追加する関数。現在の日付を元に前の年のものかどうか判断する
	// 1/4 のような日付をゼロ埋めして01/04にする
	// 例えば8/12 のような形で来たdateは 2018/08/12 にして返す

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 現在の年月を出す（goのtimeフォーマットに注意！）
	year := now.Format("2006")
	month := now.Format("1")

	// 年と月をintに変換
	// TODO エラーハンドリングする.以下も
	y, _ := strconv.Atoi(year)
	m, _ := strconv.Atoi(month)

	// スクレイピングしたデータを月と日に分ける
	date_md := strings.Split(date, "/")
	date_m, _ := strconv.Atoi(date_md[0])
	date_d, _ := strconv.Atoi(date_md[1])

	var buffer = bytes.NewBuffer(make([]byte, 0, 30))
	// スクレイピングしたデータが現在の月より先なら前の年のデータ
	// ex. 1月にスクレイピングしたデータに12月が含まれていたら前年のはず
	if date_m > m {
		buffer.WriteString(strconv.Itoa(y - 1))
	} else {
		buffer.WriteString(year)
	}
	// あらためて年/月/日の形にして返す
	buffer.WriteString("/")
	// 2桁になるようにゼロパティング
	buffer.WriteString(fmt.Sprintf("%02d", date_m))
	buffer.WriteString("/")
	// 2桁になるようにゼロパティング
	buffer.WriteString(fmt.Sprintf("%02d", date_d))
	return buffer.String()
}

func doScrape(r *http.Request, code string) (string, string, error) {

	// "HOURLY_PRICE_URL"のHDML doc取得
	doc, err := fetchWebpageDoc(r, "HOURLY_PRICE_URL", code)
	if err != nil {
		return "", "", err
	}

	// time と priceを取得
	var time, price string
	doc.Find(".stockInfoinner").Each(func(i int, s *goquery.Selection) {
		time = s.Find(".ttl1").Text()
		price = s.Find(".item1").Text()
	})
	// 必要な形に整形して返す
	d, err := getFormatedDate(time, r)
	if err != nil {
		return "", "", err // 変換できない時は戻る
	}
	p, err := getFormatedPrice(price)
	if err != nil {
		return "", "", err // 変換できない時は戻る
	}
	return d, p, err
}

func fetchWebpageDoc(r *http.Request, urlname string, code string) (*goquery.Document, error) {
	ctx := appengine.NewContext(r)
	client := urlfetch.Client(ctx)

	base_url := ""
	// リクエスト対象のURLを環境変数から読み込む
	if v := os.Getenv(urlname); v != "" {
		base_url = v
	} else {
		log.Errorf(ctx, "Failed to get base_url. '%v'", v)
		os.Exit(0)
	}

	// Request the HTML page.
	url := base_url + code
	//res, err := http.Get(url)
	res, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Failed to get resp. url: '%s', err: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Status code is error. statuscode: %d, status: %s, url: '%s'", res.StatusCode, res.Status, url)
	}

	// Load the HTML document
	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to load html doc. err: %v", err)
	}

	return doc, nil
}

func getFormatedDate(s string, r *http.Request) (string, error) {
	hour, min, err := getHourMin(s) // スクレイピングの結果から時刻を取得
	if err != nil {
		return "", err // 変換できない時は戻る
	}

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)

	d := now.Weekday()
	h := now.Hour()

	var ymd string
	switch d {
	case 1: // Monday
		if h < hour {
			// 月曜に取得したデータが現在時刻より後であればそれは前の金曜のもの
			ymd = now.AddDate(0, 0, -3).Format("2006/01/02")
		} else {
			ymd = now.Format("2006/01/02")
		}
	case 2, 3, 4, 5: // Tuesday,..Friday
		if h < hour {
			// 火~金曜に取得したデータが現在時刻より後であればそれは前日のもの
			ymd = now.AddDate(0, 0, -1).Format("2006/01/02")
		} else {
			ymd = now.Format("2006/01/02")
		}
	case 6: // Saturday
		// 土曜に取得したデータは前の金曜のもの
		ymd = now.AddDate(0, 0, -1).Format("2006/01/02")
	case 0: // Sunday
		// 日曜に取得したデータは前の金曜のもの
		ymd = now.AddDate(0, 0, -2).Format("2006/01/02")
	}
	return fmt.Sprintf("%s %02d:%02d", ymd, hour, min), err
}

func getHourMin(s string) (int, int, error) {

	//sの例1 "現在値(06:00)"
	//sの例2 "現在値(--:--)"
	re := regexp.MustCompile(`\d+:\d+`).Copy()
	t := strings.Split(re.FindString(s), ":") // ["06", "00"]
	hour, err := strconv.Atoi(t[0])
	if err != nil {
		// 変換できない時は戻る
		return 0, 0, fmt.Errorf("Failed to conv hour. data: '%s', err: %v", s, err)
	}
	hour = hour + 9 // GMT -> JST

	min, err := strconv.Atoi(t[1])
	if err != nil {
		return 0, 0, fmt.Errorf("Failed to conv min. data: '%s', err: %v", s, err)
	}
	return hour, min, err
}

func getFormatedPrice(s string) (string, error) {
	re := regexp.MustCompile(`[0-9,.]+`).Copy()
	price := re.FindString(s)
	if len(price) == 0 {
		// priceが空の時は終了
		return "", fmt.Errorf("Failed to fetch price. price: '%s'", price)
	}
	price = strings.Replace(price, ".0", "", 1)
	price = strings.Replace(price, ",", "", -1)
	return price, nil
}

func getUniqPrice(r *http.Request, prices []codePrice, existData map[string]bool) []codePrice {
	ctx := appengine.NewContext(r)

	var uniqPrices []codePrice
	for _, p := range prices {
		// codeと日付の組をmapに登録済みのデータと照合
		cd := fmt.Sprintf("%s %s", p.Code, p.Price[0])
		if !existData[cd] {
			uniqPrices = append(uniqPrices, codePrice{p.Code, p.Price})
			log.Debugf(ctx, "insert target code and price %s %v", p.Code, p.Price)
		} else {
			log.Debugf(ctx, "duplicated code and price %s %v", p.Code, p.Price)
		}
	}
	return uniqPrices
}

func writeStockpriceDaily(r *http.Request, srv *sheets.Service, prices []codePrice) (int, int) {
	ctx := appengine.NewContext(r)

	// spreadsheetに書き込む対象の行列を作成
	var matrix = make([][]interface{}, 0)
	for _, p := range prices {
		var ele = make([]interface{}, 0)
		ele = append(ele, p.Code)
		for _, v := range p.Price {
			ele = append(ele, v)
		}
		// ele ex. [8306 8/23 320 322 317 319 8068000 319.0]
		matrix = append(matrix, ele)
	}
	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		//matrix : [][]interface{} 型
		// [銘柄, 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値] * 日付
		Values: matrix,
	}

	// spreadsheetに書き込むレコードの件数
	target_num := len(matrix)
	log.Infof(ctx, "insert target num: %v", target_num)

	var MaxRetries = 5
	for attempt := 0; attempt < MaxRetries; attempt++ {
		resp, err := srv.Spreadsheets.Values.Append(DAILYPRICE_SHEETID, "daily", valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
		if err != nil {
			log.Warningf(ctx, "failed to write spreadsheet. error: %v. attempt: %d", err, attempt+1)
			time.Sleep(3 * time.Second) // 3秒待つ
			continue
		}
		status := resp.ServerResponse.HTTPStatusCode
		if status != 200 {
			log.Warningf(ctx, "HTTPstatus error. %v. attempt: %d", status, attempt+1)
			time.Sleep(3 * time.Second) // 3秒待つ
			continue
		}
		// 書き込み対象の件数と成功した件数
		log.Debugf(ctx, "succeded to write data to sheet.")
		return target_num, target_num
	}
	// 書き込み対象の件数と成功した件数(=0)
	log.Errorf(ctx, "failed to write data to sheet. reached to MaxRetries: %d", MaxRetries)
	return target_num, 0
}

func writeStockprice(srv *sheets.Service, r *http.Request, code string, date string, stockprice string) {
	ctx := appengine.NewContext(r)

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			[]interface{}{code, date, stockprice},
		},
	}
	var MaxRetries = 3
	for attempt := 0; attempt < MaxRetries; attempt++ {
		resp, err := srv.Spreadsheets.Values.Append(STOCKPRICE_SHEETID, "stockprice", valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
		if err != nil {
			log.Warningf(ctx, "Unable to write value. %v. attempt: %d", err, attempt)
			time.Sleep(3 * time.Second) // 3秒待つ
			continue
		}
		status := resp.ServerResponse.HTTPStatusCode
		if status != 200 {
			log.Warningf(ctx, "HTTPstatus error. %v. attempt: %d", status, attempt)
			time.Sleep(3 * time.Second) // 3秒待つ
			continue
		}
		return
	}
	log.Errorf(ctx, "Failed to write data to sheet. reached to MaxRetries: %d", MaxRetries)
}

func calcIncreaseRate(resp [][]interface{}, code string, num int, r *http.Request) ([]float64, error) {
	ctx := appengine.NewContext(r)

	// 直近のnum分の株価の変動率を計算する

	log.Debugf(ctx, "code: %s", code)
	var price []float64
	// 最新のデータから順番に読んでいく
	// 読み込むデータは一番下の行が最新で日付順に並んでいることが前提
	count := num
	for i := 0; i < len(resp); i++ {
		v := resp[len(resp)-1-i] // [8316 2018/08/09 15:00 4426]
		//		log.Debugf(ctx, "v: %v, v[0]: %v, v[1]: %v", v, v[0], v[1])
		if len(v) < 3 { // 銘柄, 日付, 株価の3要素が必要
			return nil, fmt.Errorf("code %s record is unsatisfied. record: %v", code, v)
		}
		if v[0] == code {
			p, err := strconv.ParseFloat(v[2].(string), 64)
			if err != nil {
				return nil, fmt.Errorf("code %s's price cannot be converted to ParseFloat. record: %v. err: %v", code, v, err)
			}
			//log.Debugf(ctx, "code: %s, date: %v, p: %v", code, v[1], p)

			price = append(price, p)

			count = count - 1
			// 指定回数分取得したらループを抜ける
			if count <= 0 {
				break
			}
		}
	}
	// price ex. [685.1 684.6 683.2 684.2 686.9 684.3 684.3]

	// rate[0] = price[0]/price[1]
	// ...
	// rate[num-2] = price[num-2]/price[num-1]
	var rate []float64
	for i := 0; i < num-1; i++ {
		if i+1 < len(price) {
			rate = append(rate, price[0]/price[i+1])
		} else {
			rate = append(rate, 0.0)
		}
	}
	// rate ex. [1.0007303534910896 1.002781030444965 1.0013154048523822 0.997379531227253 1.0011690778898146 1.0011690778898146]
	return rate, nil
}
