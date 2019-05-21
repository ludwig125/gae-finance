package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	//"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch" // 外部にhttpするため
)

// codeごとの株価比率
type codeRate struct {
	Code string
	Rate []float64
}

// 日付と終値
type dateClose struct {
	Date  string
	Close float64
}
type codeDiffRate struct {
	Code             string
	DiffCloseMoving5 float64 // 直近の終値と５日移動平均の差
	IncreasingRate   float64 // 直近の終値のその一つ前の終値との増加率
}

// codeDiffRateの要素を全てinterfaceにするメソッド
func (cdr *codeDiffRate) toInterface() []interface{} {
	var cdrIF []interface{}
	cdrIF = append(cdrIF, cdr.Code)
	cdrIF = append(cdrIF, cdr.DiffCloseMoving5)
	cdrIF = append(cdrIF, cdr.IncreasingRate)
	return cdrIF
}

type codeDiffRates []codeDiffRate

// codeDiffRatesを[][]interfaceにするメソッド
// SpreadSheetへの書き込みのためにinterface型にする必要がある
func (cdrs *codeDiffRates) toInterface() [][]interface{} {
	var cdrsIF [][]interface{}
	for _, cdr := range *cdrs {
		cdrsIF = append(cdrsIF, cdr.toInterface())
	}
	return cdrsIF
}

func main() {
	http.HandleFunc("/_ah/start", start)
	http.HandleFunc("/daily", dailyHandler)
	http.HandleFunc("/movingavg", movingAvgHandler)
	http.HandleFunc("/ensure_daily", ensureDailyDBHandler)
	http.HandleFunc("/calc", calcHandler)
	http.HandleFunc("/", indexHandler)
	appengine.Main() // Starts the server to receive requests
}

// バッチ処理のbasic_scalingを使うために /_ah/startのハンドラが必要
func start(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	log.Infof(c, "STARTING")
}

// TODO: 他のHandlerでもこれを最初に読み込むようにしたい
// 環境変数の読み込み、SheetやDBのClientの取得をする
func initialize(r *http.Request) (*sheets.Service, *sql.DB, error) {
	// GAE log
	ctx := appengine.NewContext(r)

	// read environment values
	getEnv(r)

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "failed to get sheet client. err: %v", err)
		return nil, nil, err
	}
	log.Infof(ctx, "succeeded to get sheet client")

	// cloud sql(ローカルの場合はmysql)と接続
	db, err := dialSQL(r)
	if err != nil {
		log.Errorf(ctx, "failed to open db. err: %v", err)
		return nil, nil, err
	}
	log.Infof(ctx, "succeeded to open db")

	return sheetService, db, nil
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

func dailyHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// read environment values
	getEnv(r)

	// 100件ずつ(test環境は10件)スクレイピングしてSheetに書き込み
	// 最初に環境変数を読み込む
	maxSheetInsertNum, err := strconv.Atoi(mustGetenv(r, "MAX_SHEET_INSERT"))
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
	log.Infof(ctx, "Succeeded to get sheet client")

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 以下はデバッグ用
	//now := time.Date(2019, 5, 18, 10, 11, 12, 0, time.Local)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheetService)
	// 前の日が休みの日だったら取得すべきデータがないので起動しない
	if !isPreviousBussinessday(r, now, holidayMap) {
		log.Infof(ctx, "Previous day is not business day.")
		return
	}

	// spreadsheetから銘柄コードを取得
	//codes := readCode(sheetService, r, "ichibu")
	codes := getSheetData(r, sheetService, codeSheetID, "ichibu")
	if codes == nil || len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		os.Exit(0)
	}

	// cloud sql(ローカルの場合はmysql)と接続
	db, err := dialSQL(r)
	if err != nil {
		log.Errorf(ctx, "Could not open db: %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "Succeeded to open db")

	// 書き込み対象の件数
	target := 0
	// 書き込めた件数
	inserted := 0

	// 書き込み対象のdaily の項目名
	dailyColumns := []string{"code", "date", "open", "high", "low", "close", "turnover", "modified"}

	//log.Infof(ctx, "db %T", db)
	length := len(codes)
	for begin := 0; begin < length; begin += maxSheetInsertNum {
		end := begin + maxSheetInsertNum
		if end >= length {
			end = length
		}
		// 指定された複数の銘柄単位でcodeをScrape
		// scrapeに失敗してもエラーを出して続ける
		prices, err := getEachCodesPrices(r, codes[begin:end])
		if err != nil {
			log.Warningf(ctx, "failed to scrape code. %v", err)
		}
		//log.Debugf(ctx, "prices: %v", prices)

		target += len(prices)

		// dailypriceをcloudsqlに挿入
		ins, err := insertDataStrings(r, db, "daily", dailyColumns, prices)
		if err != nil {
			log.Errorf(ctx, "failed to insert. %v", err)
			continue
		}
		inserted += ins
	}

	if target != inserted {
		log.Errorf(ctx, "failed to write all records. target: %d, inserted: %d", target, inserted)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to write all records. target: %d, inserted: %d", target, inserted)
}

// 複数銘柄についてそれぞれの株価を取得する
func getEachCodesPrices(r *http.Request, codes [][]interface{}) ([][]string, error) {
	ctx := appengine.NewContext(r)

	var codePrices [][]string

	var allErrors string
	for _, v := range codes {
		code := v[0].(string) // row's type: []interface {}. ex. [8411]
		log.Infof(ctx, "scraping code: %s", code)

		// codeごとに株価を取得
		// oneMonthPricesは,
		// [日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値]の配列が１ヶ月分入った二重配列
		oneMonthPrices, err := doScrapeDaily(r, code)
		if err != nil {
			//log.Infof(ctx, "err: %v", err)
			allErrors += fmt.Sprintf("[code: %s %v]\n", code, err)
			continue
		}

		// ["日付", "始値"...],["日付", "始値"...],...を１行ずつ展開
		for _, oneDayPrices := range oneMonthPrices {
			// ["日付", "始値"...]の配列の先頭に銘柄codeを追加
			oneDayCodePrices := append([]string{code}, oneDayPrices...)
			codePrices = append(codePrices, oneDayCodePrices)
		}
		time.Sleep(1 * time.Second) // 1秒待つ
	}
	if allErrors != "" {
		// 複数の銘柄で起きたエラーをまとめて出力
		return codePrices, fmt.Errorf("%s", allErrors)
	}
	return codePrices, nil
}

func movingAvgHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// get environment var, sheet, db
	sheet, db, err := initialize(r)
	if err != nil {
		log.Errorf(ctx, "failed to initialize. err: %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to initialize. got environment var, sheet, db.")
	//log.Infof(ctx, "%v %v", sheet, db)

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheet)
	// 前の日が休みの日だったら取得すべきデータがないので起動しない
	if !isPreviousBussinessday(r, now, holidayMap) {
		log.Infof(ctx, "Previous day is not business day.")
		return
	}

	// test環境ではデータの存在する最新の日付に合わせる
	previousBussinessDay := "2019/05/16"
	// prod環境の場合は、直近の取引日を取得する
	// 一日前から順番に見ていって、直近の休日ではない日を取引日として設定する
	if runEnv != "test" {
		// 直近の営業日を取得
		previos, err := getPreviousBussinessDay(now, holidayMap)
		if err != nil {
			log.Errorf(ctx, "failed to getPreviousBussinessDay. %v", err)
			os.Exit(0)
		}
		previousBussinessDay = previos
	}
	log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)

	// 最新の日付にある銘柄を取得
	codes := fetchSelectResult(r, db,
		"SELECT code FROM daily WHERE date = (SELECT date FROM daily ORDER BY date DESC LIMIT 1);")

	for _, code := range codes {
		// 直近 100日分最近から順にソートして取得
		// TODO: あとで100に直す
		dcs, err := getOrderedDateCloses(r, db, code.(string), previousBussinessDay, 100)
		if err != nil {
			log.Errorf(ctx, "failed to getOrderedDateCloses. code: %s, err: %v", code, err)
			os.Exit(0) // TODO: あとで消すか検討
		}

		// DBから取得できた日付のリスト
		var dateList []string
		for date := 0; date < len(dcs); date++ {
			dateList = append(dateList, dcs[date].Date)
		}
		log.Infof(ctx, "moving average target code %s, dateSize: %d", code.(string), len(dateList))

		// 取得対象の移動平均
		mvAvgList := []int{3, 5, 7, 10, 20, 60, 100}
		// (日付;移動平均)のMapを3, 5, 7,...ごとに格納したMap
		daysDateMovingMap := make(map[int]map[string]float64)
		for _, d := range mvAvgList {
			daysDateMovingMap[d] = movingAverage(r, dcs, d)
		}
		// 移動平均をDBに書き込み
		insertMovingAvg(r, db, "movingavg", code.(string), dateList, daysDateMovingMap)
	}

	// 以下のgoroutineを実行したら以下のエラーが発生
	// A problem was encountered with the process that handled this request, causing it to exit. This is likely to cause a new process to be used for the next request to your application. (Error code 204)

	//	var wg sync.WaitGroup
	//	wg.Add(len(codes))
	//	for _, code := range codes {
	//		go func(code string) {
	//			defer wg.Done()
	//			// 直近 100日分新しい順にソートして取得
	//			dcs := orderedDateClose(code, 100)
	//
	//			// DBから取得できた日付のリスト
	//			var dateList []string
	//			for date := 0; date < len(dcs); date++ {
	//				dateList = append(dateList, dcs[date].Date)
	//			}
	//
	//			// 取得対象の移動平均
	//			mvAvgList := []int{5, 20, 60, 100}
	//			// (日付;移動平均)のMapを5, 20,...ごとに格納したMap
	//			daysDateMovingMap := make(map[int]map[string]float64)
	//			for _, d := range mvAvgList {
	//				// 移動平均の計算
	//				daysDateMovingMap[d] = movingAverage(dcs, d)
	//			}
	//			// 移動平均をDBに書き込み
	//			insertMovingAvg(r, db, "movingavg", code, dateList, daysDateMovingMap)
	//
	//		}(code.(string)) // codeはinterface型なのでキャストする
	//	}
	//	wg.Wait()

}

func movingAverage(r *http.Request, dcs []dateClose, avgDays int) map[string]float64 {
	// GAE log
	//ctx := appengine.NewContext(r)

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
		//var sum int64
		var sum float64
		for i := date; i < date+days; i++ {
			//log.Infof(ctx, "movingAverage dcs[i].Close %v", dcs[i].Close)
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

func calcHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// get environment var, sheet, db
	sheet, db, err := initialize(r)
	if err != nil {
		log.Errorf(ctx, "failed to initialize. err: %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to initialize. got environment var, sheet, db.")
	//log.Infof(ctx, "%v %v", sheet, db)

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheet)
	// 前の日が休みの日だったら取得すべきデータがないので起動しない
	if !isPreviousBussinessday(r, now, holidayMap) {
		log.Infof(ctx, "Previous day is not business day.")
		return
	}

	// test環境ではデータの存在する最新の日付に合わせる
	previousBussinessDay := "2019/05/16"
	// prod環境の場合は、直近の取引日を取得する
	// 一日前から順番に見ていって、直近の休日ではない日を取引日として設定する
	if runEnv != "test" {
		// 直近の営業日を取得
		previos, err := getPreviousBussinessDay(now, holidayMap)
		if err != nil {
			log.Errorf(ctx, "failed to getPreviousBussinessDay. %v", err)
			os.Exit(0)
		}
		previousBussinessDay = previos
	}
	log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)

	// ５日移動平均からの差分と、前日からの増加率を返す関数
	calcDiffCloseMoving5IncreasingRate := func(code string) (float64, float64, error) {
		dcs, err := getOrderedDateCloses(r, db, code, previousBussinessDay, 2)
		if err != nil {
			log.Errorf(ctx, "failed to getOrderedDateCloses. code: %s, err: %v", code, err)
			return 0.0, 0.0, err
		}
		log.Infof(ctx, "dcs: %v", dcs)
		log.Infof(ctx, "dcs rate: %v %f", dcs[0].Close, float64(dcs[0].Close)/float64(dcs[1].Close))

		moving5, err := getMoving5(r, db, code, dcs[0].Date)
		if err != nil {
			log.Errorf(ctx, "failed to getMoving5. code: %s, err: %v", code, err)
			return 0.0, 0.0, err
		}

		// 直近の日付の終値と５日移動平均の差
		diffCloseMoving5 := float64(dcs[0].Close) - moving5
		// 直近の日付の終値のその一つ前の日の終値に対する増加率
		increasingRate := float64(dcs[0].Close) / float64(dcs[1].Close)

		return diffCloseMoving5, increasingRate, nil
	}

	// 最新の日付にある銘柄を取得
	codes := fetchSelectResult(r, db,
		"SELECT code FROM daily WHERE date = (SELECT date FROM daily ORDER BY date DESC LIMIT 1);")

	// debug用
	// codes := []interface{}{}
	// codesStr := []string{"6758", "7201", "8058", "9432"}
	// for _, v := range codesStr {
	// 	codes = append(codes, v)
	// }
	log.Infof(ctx, "%v", codes)

	cdrs := codeDiffRates{}
	for _, code := range codes {
		diff, rate, err := calcDiffCloseMoving5IncreasingRate(code.(string))
		if err != nil {
			log.Errorf(ctx, "failed to calcDiffCloseMoving5IncreasingRate. code: %s, err: %v", code, err)
			os.Exit(0)
		}
		//log.Infof(ctx, "%f %f", diff, rate)
		cdrs = append(cdrs, codeDiffRate{Code: code.(string), DiffCloseMoving5: diff, IncreasingRate: rate})
	}

	// 「終値-５日移動平均」の差が大きい順に並び替え
	sort.SliceStable(cdrs, func(i, j int) bool {
		return cdrs[i].DiffCloseMoving5 > cdrs[j].DiffCloseMoving5
	})

	// 事前にSheetをclear
	sheetName := "kahanshin"
	if err := clearSheet(sheet, calcSheetID, sheetName); err != nil {
		log.Errorf(ctx, "failed to clearSheet. sheetID: %s, sheetName: %s", calcSheetID, sheetName)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to clearSheet. sheetID: %s, sheetName: %s", calcSheetID, sheetName)

	// Sheetへ書き込み
	// [][]interface{}型に直してwriteSheetに渡す
	cdrsIF := cdrs.toInterface()
	if err := writeSheet(sheet, calcSheetID, sheetName, cdrsIF); err != nil {
		log.Errorf(ctx, "failed to writeSheet. sheetID: %s, sheetName: %s", calcSheetID, sheetName)
		log.Infof(ctx, "error data: %v", cdrsIF)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to writeSheet. sheetID: %s, sheetName: %s", calcSheetID, sheetName)
}

// codeと取得する件数と検索する日付を与えると、
// 日付と終値の構造体を直近の日付順にして配列で返す関数
// 取得する件数 limit: 指定しない場合は0
// 検索する日付 latestDate: 指定しない場合は空. 指定した場合はその日付を最新のものとして検索
func getOrderedDateCloses(r *http.Request, db *sql.DB, code string, latestDate string, limit int) ([]dateClose, error) {
	// TODO: ログ出さないならパラメータのrは不要
	//ctx := appengine.NewContext(r)
	limitStr := ""
	if limit != 0 {
		limitStr = fmt.Sprintf("LIMIT %d", limit)
	}

	latestDateStr := ""
	if latestDate != "" {
		latestDateStr = fmt.Sprintf("AND date <= '%s'", latestDate)
	}

	dbRet := fetchSelectResult(r, db, fmt.Sprintf(
		"SELECT date, close FROM daily WHERE code = %s %s ORDER BY date DESC %s;", code, latestDateStr, limitStr))
	if len(dbRet) == 0 {
		return nil, fmt.Errorf("no selected data")
	}

	var dateCloses []dateClose
	// 日付と終値の２つを取得
	for i := 0; i < len(dbRet); i += 2 {
		// float64型数値に変換
		// 株価には小数点が入っていることがあるのでfloatで扱う
		//c, _ := strconv.Atoi(dbRet[i+1].(string))
		//c, err := strconv.ParseInt(dbRet[i+1].(string), 10, 64)
		c, err := strconv.ParseFloat(dbRet[i+1].(string), 64)
		if err != nil {
			return nil, fmt.Errorf("failed to ParseFloat. %v", err)
		}
		dateCloses = append(dateCloses, dateClose{Date: dbRet[i].(string), Close: c})
		//log.Infof(ctx, "c: %v", c)
		//log.Infof(ctx, "dbRet[i] %s dbRet[i+1] %s", dbRet[i], dbRet[i+1])
	}
	//log.Infof(ctx, "dateCloses %v", dateCloses)
	return dateCloses, nil
}

// 銘柄コードと日付を渡すと該当の５日移動平均を返す
func getMoving5(r *http.Request, db *sql.DB, code string, date string) (float64, error) {
	//ctx := appengine.NewContext(r)

	dbRet := fetchSelectResult(r, db, fmt.Sprintf(
		"SELECT moving5 FROM movingavg WHERE code = %s and date = '%s';", code, date))
	if len(dbRet) == 0 {
		return 0.0, fmt.Errorf("no selected data")
	}

	// []interface {}型のdbRetをfloat64に変換
	moving5, _ := strconv.ParseFloat(dbRet[0].(string), 64)

	//log.Infof(ctx, "%f", moving5)
	return moving5, nil
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
	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheetService)
	// 前の日が休みの日だったら取得すべきデータがないので起動しない
	if !isPreviousBussinessday(r, now, holidayMap) {
		log.Infof(ctx, "Previous day is not business day.")
		return
	}

	// spreadsheetから銘柄コードを取得
	//codes := readCode(sheetService, r, "code")
	codes := getSheetData(r, sheetService, codeSheetID, "code")
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
	resp := getSheetData(r, sheetService, stockPriceSheetID, "stockprice")
	if resp == nil {
		log.Infof(ctx, "No data")
		return
	}

	// 全codeの株価比率
	var wholeCodeRate []codeRate
	for _, row := range codes {
		code := row[0].(string)
		//直近7時間の増減率を取得する
		rate, err := calcIncreaseRate(resp, code, 7, r)
		if err != nil {
			log.Warningf(ctx, "%v\n", err)
			continue
		}
		wholeCodeRate = append(wholeCodeRate, codeRate{code, rate})
	}
	log.Infof(ctx, "count whole code %v\n", len(wholeCodeRate))

	// 一つ前との比率が一番大きいもの順にソート
	sort.SliceStable(wholeCodeRate, func(i, j int) bool { return wholeCodeRate[i].Rate[0] > wholeCodeRate[j].Rate[0] })
	fmt.Fprintln(w, wholeCodeRate)

	// 事前にrateのシートをclear
	sheetName := "rate"
	if err := clearSheet(sheetService, rateSheetID, sheetName); err != nil {
		log.Errorf(ctx, "failed to clearSheet. sheetID: %s, sheetName: %s", rateSheetID, sheetName)
		os.Exit(0)
	}

	// 株価の比率順にソートしたものを書き込み
	writeRate(sheetService, r, wholeCodeRate, rateSheetID, "rate")
}

var (
	codeSheetID       string
	runEnv            string
	holidaySheetID    string
	stockPriceSheetID string
	dailyRateSheetID  string
	rateSheetID       string
	calcSheetID       string
)

func getEnv(r *http.Request) {
	ctx := appengine.NewContext(r)
	// 環境変数から読み込む

	codeSheetID = mustGetenv(r, "CODE_SHEETID")
	runEnv = mustGetenv(r, "ENV")
	if runEnv != "test" && runEnv != "prod" {
		// runEnvがprodでもtestでもない場合は異常終了
		log.Errorf(ctx, "ENV must be 'test' or 'prod': %v", runEnv)
		os.Exit(0)
	}
	holidaySheetID = mustGetenv(r, "HOLIDAY_SHEETID")
	stockPriceSheetID = mustGetenv(r, "STOCKPRICE_SHEETID")
	dailyRateSheetID = mustGetenv(r, "DAILYRATE_SHEETID")
	rateSheetID = mustGetenv(r, "RATE_SHEETID")
	calcSheetID = mustGetenv(r, "CALC_SHEETID")
}

// spreadsheetの'holiday' sheetを読み取って、{"2019/01/01", true}のような祝日のMapを作成して返す
func getHolidaysFromSheet(r *http.Request, srv *sheets.Service) map[string]bool {
	ctx := appengine.NewContext(r)
	// 'holiday' sheet を読み取り
	// sheetには「2019/01/01」の形式の休日が縦一列になっていることを想定している
	// 東京証券取引所の休日: https://www.jpx.co.jp/corporate/calendar/index.html
	holidays := getSheetData(r, srv, holidaySheetID, "holiday")
	if holidays == nil || len(holidays) == 0 {
		log.Infof(ctx, "Failed to get holidays.")
		os.Exit(0)
	}
	holidayMap := map[string]bool{}
	// [][]interface{}型のholidaysを読み取り、holidayはtrueとなるMapを作成
	for _, row := range holidays {
		holiday := row[0].(string)
		holidayMap[holiday] = true
	}
	return holidayMap
}

func doScrapeDaily(r *http.Request, code string) ([][]string, error) {
	// "DAILY_PRICE_URL"のHDML doc取得
	doc, err := fetchWebpageDoc(r, "DAILY_PRICE_URL", code)
	if err != nil {
		return nil, err
	}

	// date と priceを取得
	var datePrice [][]string
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
			// 終値 5,430 -> 5430 のように修正
			arr = append(arr, strings.Replace(s2.Text(), ",", "", -1))
		})
		// 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値を一行ごとに格納
		datePrice = append(datePrice, arr)
	})
	if len(datePrice) == 0 {
		return nil, fmt.Errorf("%s no data", code)
	}
	if len(datePrice[0]) != 7 {
		// 以下の７要素を取れなかったら何かおかしい
		// 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値
		// リダイレクトされて別のページに飛ばされている可能性もある
		// 失敗した銘柄を返す
		return nil, fmt.Errorf("%s doesn't have enough elems", code)
	}
	return datePrice, nil
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
	fetchedMonthDate := strings.Split(date, "/")
	fetchedMonth, _ := strconv.Atoi(fetchedMonthDate[0])
	fetchedDate, _ := strconv.Atoi(fetchedMonthDate[1])

	var buffer = bytes.NewBuffer(make([]byte, 0, 30))
	// スクレイピングしたデータが現在の月より先なら前の年のデータ
	// ex. 1月にスクレイピングしたデータに12月が含まれていたら前年のはず
	if fetchedMonth > m {
		buffer.WriteString(strconv.Itoa(y - 1))
	} else {
		buffer.WriteString(year)
	}
	// あらためて年/月/日の形にして返す
	buffer.WriteString("/")
	// 2桁になるようにゼロパティング
	buffer.WriteString(fmt.Sprintf("%02d", fetchedMonth))
	buffer.WriteString("/")
	// 2桁になるようにゼロパティング
	buffer.WriteString(fmt.Sprintf("%02d", fetchedDate))
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
	d, err := getFormatedDate(time)
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

	baseURL := ""
	// リクエスト対象のURLを環境変数から読み込む
	if v := os.Getenv(urlname); v != "" {
		baseURL = v
	} else {
		log.Errorf(ctx, "Failed to get baseURL. '%v'", v)
		os.Exit(0)
	}

	// Request the HTML page.
	url := baseURL + code
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

func getFormatedDate(s string) (string, error) {
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
		resp, err := srv.Spreadsheets.Values.Append(stockPriceSheetID, "stockprice", valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
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

// スプレッドシート上の全銘柄について本日分のデータがDBに書き込まれているか確認
func ensureDailyDBHandler(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

	// 環境変数を最初に読み込み
	getEnv(r)

	log.Infof(ctx, "ensure daily data")

	// spreadsheetのclientを取得
	sheetService, err := getSheetClient(r)
	if err != nil {
		log.Errorf(ctx, "err: %v", err)
		os.Exit(0)
	}

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 以下はデバッグ用
	//now := time.Date(2019, 5, 18, 10, 11, 12, 0, time.Local)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheetService)
	// 前の日が休みの日だったら取得すべきデータがないので起動しない
	if !isPreviousBussinessday(r, now, holidayMap) {
		log.Infof(ctx, "Previous day is not business day.")
		return
	}

	// test環境ではデータの存在する最新の日付に合わせる
	previousBussinessDay := "2019/05/16"
	// prod環境の場合は、直近の取引日を取得する
	// 一日前から順番に見ていって、直近の休日ではない日を取引日として設定する
	if runEnv != "test" {
		// 直近の営業日を取得
		previos, err := getPreviousBussinessDay(now, holidayMap)
		if err != nil {
			log.Errorf(ctx, "failed to getPreviousBussinessDay. %v", err)
			os.Exit(0)
		}
		previousBussinessDay = previos
	}
	log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)

	// あとで全銘柄と比較するためにDBの直近の取引日のデータに含まれる銘柄を取得してmapに格納
	codesInDb := func() map[int]bool {
		// cloud sql(ローカルの場合はmysql)と接続
		db, err := dialSQL(r)
		if err != nil {
			log.Errorf(ctx, "Could not open db: %v", err)
			os.Exit(0)
		}
		log.Infof(ctx, "Succeded to open db")

		query := fmt.Sprintf("SELECT code FROM daily WHERE date = '%s'", previousBussinessDay)
		dbRet := fetchSelectResult(r, db, query)
		log.Infof(ctx, "fetched %d codes from 'daily' in db. target date: %s", len(dbRet), previousBussinessDay)

		// あとで全銘柄と比較するためにmapに格納
		dbCodesMap := map[int]bool{}
		for _, v := range dbRet {
			code, _ := strconv.Atoi(v.(string)) // int型に変換
			dbCodesMap[code] = true
		}
		//log.Infof(ctx, "dbcodes %v", dbCodesMap)
		return dbCodesMap
	}
	dbCodesMap := codesInDb()

	// spreadsheetから銘柄コードを取得
	codes := getSheetData(r, sheetService, codeSheetID, "ichibu")
	if codes == nil || len(codes) == 0 {
		log.Errorf(ctx, "failed to fetch sheetdata. err: '%v'.", codes)
		os.Exit(0)
	}
	log.Infof(ctx, "fetched %d codes from 'ichibu' in sheet", len(codes))
	//	// 全銘柄分がsheetにあるか確認する
	//	var notExistInSheet []int
	//	全銘柄分がdbにあるか確認する
	var notExistInDb []int
	for _, v := range codes {
		code, _ := strconv.Atoi(v[0].(string))
		if !dbCodesMap[code] {
			// dbになければnotExistInDbにその銘柄を追加
			notExistInDb = append(notExistInDb, code)
		}
	}

	if len(notExistInDb) != 0 {
		log.Errorf(ctx, "failed to write all codes data to db. unmatched!! not exist in db: %v", notExistInDb)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to write all %d codes data to db.", len(dbCodesMap))
}

// 与えられた日付の前日が取引日かどうかを判定する関数
// 実行時の日付time.Now().In(jst)と休日のMapを渡す
func isPreviousBussinessday(r *http.Request, t time.Time, holidayMap map[string]bool) bool {
	ctx := appengine.NewContext(r)

	log.Infof(ctx, "Is previous day Bussinessday? Received date: %v", t)
	if runEnv == "test" {
		// test環境は常にtrue
		log.Infof(ctx, "This is test env. no need to check.")
		return true
	}

	// 以下はprodの場合

	// 直近の営業日を取得
	previousBussinessDay, err := getPreviousBussinessDay(t, holidayMap)
	if err != nil {
		log.Errorf(ctx, "failed to getPreviousBussinessDay. %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "previous BussinessDay %s", previousBussinessDay)

	// 日付を１日ずらすためにtime.Time型に修正してから計算
	previousTime, _ := time.Parse("2006/01/02", previousBussinessDay)
	previousTimeNextDay := previousTime.AddDate(0, 0, 1)
	if t.Format("2006/01/02") == previousTimeNextDay.Format("2006/01/02") {
		log.Infof(ctx, "previous day is BussinessDay.")
		return true
	}
	return false
}
