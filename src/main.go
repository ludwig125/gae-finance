package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"reflect"
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

// 可変長引数a, b, c...が a > b > cの順番のときにtrue
// TODO: 型を汎用的にしたい。このコードの置く場所変えたい
func isAGreaterThanB(params ...float64) bool {
	max := params[0]
	for m := 1; m < len(params); m++ {
		if max > params[m] {
			max = params[m]
			continue
		}
		return false
	}
	return true
}

// 可変長引数a, b, c...が a >= b >= cの順番のときにtrue
// TODO: 型を汎用的にしたい。このコードの置く場所変えたい
func isAGreaterThanOrEqualToB(params ...float64) bool {
	max := params[0]
	for m := 1; m < len(params); m++ {
		if max >= params[m] {
			max = params[m]
			continue
		}
		return false
	}
	return true
}

// 任意の構造体、または構造体のポインタを引数にとって、
// 構造体のフィールドを全てinterface{}型にしてスライスに詰めて返す関数
// 構造体の中に構造体があっても対応できる
// 参考： https://play.golang.org/p/UJ-lrN2Wjfr
func toInterfaceSlice(v interface{}) []interface{} {
	var vs []interface{}

	rv := reflect.ValueOf(v)
	// パラメータvが構造体のポインタのときはElemでポインタの指している先の値を取得する
	if rv.Kind() == reflect.Ptr {
		// vが構造体のポインタの時はここを通る
		rv = reflect.ValueOf(v).Elem()
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if rv.Field(i).Kind() == reflect.Struct {
			// フィールドがstructの場合は再帰でinterfaceのSliceを取得して後ろにつなげる
			sl := toInterfaceSlice(rv.Field(i).Interface())
			vs = append(vs, sl...)
		} else {
			// フィールドがStringメソッドを持っていたらそれを使う
			f := rv.Field(i).MethodByName("String")
			// Stringメソッドを持っている場合はfの種別がFuncになる
			if f.Kind() == reflect.Func {
				// Stringメソッドを使う
				vs = append(vs, f.Call(nil)[0].Interface())
			} else {
				vs = append(vs, rv.Field(i).Interface())
			}
		}
	}
	return vs
}

// 4: PPP : 5 > 20 > 60 > 100
// 3: semiPPP : 5 > 20 > 60
// 2: oppositeSemiPPP : 60 > 20 > 5
// 1: oppositePPP : 100 > 60 > 20 > 5
// 0: NON : other
type pppKind int

const (
	non pppKind = iota
	oppositePPP
	oppositeSemiPPP
	semiPPP
	ppp
)

// constのString変換メソッド
func (p pppKind) String() string {
	return [5]string{"non", "oppositePPP", "oppositeSemiPPP", "semiPPP", "ppp"}[p]
}

type movings struct {
	Moving5   float64 // ５日移動平均
	Moving20  float64
	Moving60  float64
	Moving100 float64
}

func (m movings) calcPPPKind() pppKind {
	// moving5 > moving20 > moving60 > moving100の並びのときPPP
	if isAGreaterThanB(m.Moving5, m.Moving20, m.Moving60, m.Moving100) {
		return ppp
	}
	if isAGreaterThanB(m.Moving5, m.Moving20, m.Moving60) {
		return semiPPP
	}
	// 条件の厳しい順にしないとゆるい方(oppositeSemiPPP)に先に適合してしまうので注意
	if isAGreaterThanB(m.Moving100, m.Moving60, m.Moving20, m.Moving5) {
		return oppositePPP
	}
	if isAGreaterThanB(m.Moving60, m.Moving20, m.Moving5) {
		return oppositeSemiPPP
	}
	return non
}

type pppInfo struct {
	PPP     pppKind
	Movings movings
}

func (p *pppInfo) Interface() []interface{} {
	return toInterfaceSlice(p)
}

// 前日の終値と前々日の終値が５日移動平均を横切る場合のその増加率
type kahanshinInfo struct {
	BeforePreviousClose float64 // 直近のその一つ前の日の終値
	PreviousClose       float64 // 直近の終値
	IncreasingRate      float64 // 直近の終値のその一つ前の終値との増加率
}

// 要素を全てinterfaceにしたスライスを返すメソッド
func (k *kahanshinInfo) Interface() []interface{} {
	return toInterfaceSlice(k)
}

type marketInfo struct {
	Code          string // 銘柄
	Date          string // 直近の日付
	PPPInfo       pppInfo
	KahanshinInfo kahanshinInfo
}

// 要素を全てinterfaceにしたスライスを返すメソッド
func (m *marketInfo) Interface() []interface{} {
	return toInterfaceSlice(m)
}

type marketInfos []marketInfo

func (ms *marketInfos) Interface() [][]interface{} {
	var msi [][]interface{}
	for _, m := range *ms {
		msi = append(msi, m.Interface())
	}
	return msi
}

func main() {
	// TODO: Handerごとに開始と終了のログを出して、実行時間も表示する
	http.HandleFunc("/_ah/start", start)
	http.HandleFunc("/daily", dailyHandler)
	http.HandleFunc("/movingavg", movingAvgHandler)
	http.HandleFunc("/ensure_daily", ensureDailyDBHandler)
	http.HandleFunc("/calc", calcHandler)
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/connect_db", connectDBHandler)
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

// ブラウザでDBに接続できるか確認するためのHandler
func connectDBHandler(w http.ResponseWriter, r *http.Request) {
	// read environment values
	getEnv(r)

	// cloud sql(ローカルの場合はmysql)と接続
	db, err := dialSQL(r)
	if err != nil {
		fmt.Fprintf(w, "Could not open db: %v\n", err)
		return
	}
	fmt.Fprintln(w, "Succeeded to open db")

	showDatabases(w, db)

	countDaily, err := selectTable(r, db, "SELECT COUNT(*) FROM daily;")
	if err != nil {
		fmt.Fprintf(w, "Failed to select table %v\n", err)
	}
	fmt.Fprintf(w, "Total 'daily' records: %s\n", countDaily[0])
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
		ins, err := insertDB(r, db, "daily", dailyColumns, prices)
		if err != nil {
			log.Errorf(ctx, "failed to insertDB. %v", err)
			continue
		}
		inserted += ins
	}

	if target != inserted {
		log.Errorf(ctx, "failed to write all records. target: %d, inserted: %d", target, inserted)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to write all records. target: %d, inserted: %d", target, inserted)
	log.Infof(ctx, "done dailyHandler.")
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
	codes, err := selectTable(r, db,
		"SELECT code FROM daily WHERE date = (SELECT date FROM daily ORDER BY date DESC LIMIT 1);")
	if err != nil {
		log.Errorf(ctx, "failed to selectTable %v", err)
		os.Exit(0)
	}
	targetRecordNum := 0
	insertedRecordNum := 0
	for _, code := range codes {
		// 直近 100日分最近から順にソートして取得
		dcs, err := getOrderedDateCloses(r, db, code, previousBussinessDay, 100)
		if err != nil {
			log.Errorf(ctx, "failed to getOrderedDateCloses. code: %s, err: %v", code, err)
			os.Exit(0)
		}

		// 取得対象の移動平均
		movingDayList := []int{3, 5, 7, 10, 20, 60, 100}
		// (日付;移動平均)のMapを3, 5, 7,...ごとに格納したMap
		daysDateMovingMap := make(map[int]map[string]float64)
		for _, d := range movingDayList {
			daysDateMovingMap[d] = movingAverage(r, dcs, d)
		}

		// DBから取得できた日付はdcs[date].Dateで取れる
		// code, date, moving3, moving5, moving7...のレコードを[][]stringの形にする
		var codeDateMovings [][]string
		for dateNum := 0; dateNum < len(dcs); dateNum++ {
			date := dcs[dateNum].Date
			var codeDateMoving []string
			codeDateMoving = append(codeDateMoving, code)
			codeDateMoving = append(codeDateMoving, date)
			// movingDayList(3, 5, 7, 10, 20...)の順に対象の移動平均をスライスに詰める
			for _, movingDay := range movingDayList {
				codeDateMoving = append(codeDateMoving, fmt.Sprintf("%f", daysDateMovingMap[movingDay][date]))
			}
			codeDateMovings = append(codeDateMovings, codeDateMoving)
		}
		log.Infof(ctx, "moving average target code %s, dateSize: %d", code, len(codeDateMovings))
		//log.Debugf(ctx, "codeDateMovings %v", codeDateMovings)
		movingavgColumns := []string{"code", "date", "moving3", "moving5", "moving7", "moving10", "moving20", "moving60", "moving100"}

		// 移動平均をDBに書き込み
		// movingavgをcloudsqlに挿入
		ins, err := insertDB(r, db, "movingavg", movingavgColumns, codeDateMovings)
		targetRecordNum += ins
		if err != nil {
			log.Errorf(ctx, "failed to insertDB. %v", err)
			continue
		}
		insertedRecordNum += ins
	}
	if targetRecordNum != insertedRecordNum {
		log.Errorf(ctx, "failed to write all records. target: %d, inserted: %d", targetRecordNum, insertedRecordNum)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to write all records. target: %d, inserted: %d", targetRecordNum, insertedRecordNum)
	log.Infof(ctx, "done movingAvgHandler.")

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
	//			movingDayList := []int{5, 20, 60, 100}
	//			// (日付;移動平均)のMapを5, 20,...ごとに格納したMap
	//			daysDateMovingMap := make(map[int]map[string]float64)
	//			for _, d := range movingDayList {
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
	}
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

	jst, _ := time.LoadLocation("Asia/Tokyo")
	now := time.Now().In(jst)
	// 休日データを取得
	holidayMap := getHolidaysFromSheet(r, sheet)

	// TODO: あとでコメント外すか考える
	// // 前の日が休みの日だったら取得すべきデータがないので起動しない
	// if !isPreviousBussinessday(r, now, holidayMap) {
	// 	log.Infof(ctx, "Previous day is not business day.")
	// 	return
	// }

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
	codes, err := selectTable(r, db,
		"SELECT code FROM daily WHERE date = (SELECT date FROM daily ORDER BY date DESC LIMIT 1);")
	if err != nil {
		log.Errorf(ctx, "failed to selectTable %v", err)
		os.Exit(0)
	}
	// debug用
	// codes := []interface{}{}
	// codesStr := []string{"6758", "7201", "8058", "9432"}
	// for _, v := range codesStr {
	// 	codes = append(codes, v)
	// }
	log.Infof(ctx, "codes %v", codes)

	// 移動平均線の並びからPPPの種類を判定
	// calcPPP := func(code string) (pppInfo, error) {
	// 	m, err := getMovings(r, db, code, previousBussinessDay)
	// 	if err != nil {
	// 		return pppInfo{}, err
	// 	}
	// 	return pppInfo{m.calcPPPKind(), m}, nil
	// }

	calcPPPChan := func(code string) (chan pppInfo, error) {
		pppInfoChan := make(chan pppInfo)
		go func() {
			defer close(pppInfoChan)
			m, _ := getMovings(r, db, code, previousBussinessDay)
			// TODO: error handling
			pppInfoChan <- pppInfo{m.calcPPPKind(), m}
		}()
		return pppInfoChan, nil
	}

	// 前日の終値と前々日の終値が５日移動平均を横切ったものについてその変動率を返す
	calcKahanshin := func(code string, moving5 float64) (kahanshinInfo, error) {
		// 前日と前々日の終値を取得
		dcs, err := getOrderedDateCloses(r, db, code, previousBussinessDay, 2)
		if err != nil {
			return kahanshinInfo{}, fmt.Errorf("failed to getOrderedDateCloses. code: %s, err: %v", code, err)
		}

		// 陽線または陰線で横切る場合は増加率を返す
		// 前日終値>５日移動平均>前々日終値 または 前々日終値>５日移動平均>前日終値
		if isAGreaterThanOrEqualToB(dcs[0].Close, moving5, dcs[1].Close) || isAGreaterThanOrEqualToB(dcs[1].Close, moving5, dcs[0].Close) {
			return kahanshinInfo{dcs[1].Close, dcs[0].Close, dcs[0].Close / dcs[1].Close}, nil
		}
		return kahanshinInfo{dcs[1].Close, dcs[0].Close, 0.0}, nil
	}

	nowTime := time.Now().UTC()
	mis := marketInfos{}
	for _, code := range codes {
		// p, err := calcPPP(code)
		// if err != nil {
		// 	log.Errorf(ctx, "failed to calcPPP. code: %s, err: %v", code, err)
		// 	// os.Exit(0) // TODO: 一個でも取れないと失敗なのは嫌なのでContinueにした。あとで検討(retryとか)
		// 	continue
		// }

		// log.Infof(ctx, "succeeded to calcPPP. code: %s", code)

		// k, err := calcKahanshin(code, p.Movings.Moving5)
		// if err != nil {
		// 	log.Errorf(ctx, "failed to calcKahanshin. code: %s, err: %v", code, err)
		// 	// os.Exit(0) // TODO: 一個でも取れないと失敗なのは嫌なのでContinueにした。あとで検討(retryとか)
		// 	continue
		// }
		// log.Debugf(ctx, "calcKahanshin %v. code: %s", k, code)

		pc, err := calcPPPChan(code)
		if err != nil {
			log.Errorf(ctx, "failed to calcPPPChan. code: %s, err: %v", code, err)
		}
		pcContent := <-pc
		k, err := calcKahanshin(code, pcContent.Movings.Moving5)
		if err != nil {
			log.Errorf(ctx, "failed to calcKahanshinChan. code: %s, err: %v", code, err)
			// os.Exit(0) // TODO: 一個でも取れないと失敗なのは嫌なのでContinueにした。あとで検討(retryとか)
			continue
		}
		log.Infof(ctx, "succeeded to calcKahanshin. code: %s", code)
		// TODO: どうするかあとで考える
		// 	if k.IncreasingRate == 0.0 {
		// 		log.Debugf(ctx, "moving5 is not between closes. code: %s", code)
		// 		continue
		// 	}

		//mi := marketInfo{Code: code, Date: previousBussinessDay, PPPInfo: p, KahanshinInfo: k}
		mi := marketInfo{Code: code, Date: previousBussinessDay, PPPInfo: pcContent, KahanshinInfo: k}
		mis = append(mis, mi)
	}
	log.Infof(ctx, "Elapsed time %v", time.Since(nowTime))
	// 「前日終値/前々日終値」の増加率が大きい順に並び替え
	sort.SliceStable(mis, func(i, j int) bool {
		return mis[i].KahanshinInfo.IncreasingRate > mis[j].KahanshinInfo.IncreasingRate
	})
	// pppKindの定義順に並び替え
	sort.SliceStable(mis, func(i, j int) bool {
		return mis[i].PPPInfo.PPP > mis[j].PPPInfo.PPP
	})

	// Sheetへ書き込みするために[][]interface{}型に直す
	misi := mis.Interface()
	log.Infof(ctx, "trying to write sheet")
	if err := clearAndWriteSheet(sheet, calcSheetID, "market", misi); err != nil {
		log.Errorf(ctx, "failed to clearAndWriteSheet. %v", err)
		os.Exit(0)
	}
	log.Infof(ctx, "succeeded to write sheet")

	log.Infof(ctx, "done calcHandler.")
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

	dbRet, err := selectTable(r, db, fmt.Sprintf(
		"SELECT date, close FROM daily WHERE code = %s %s ORDER BY date DESC %s;", code, latestDateStr, limitStr))
	if err != nil {
		return nil, fmt.Errorf("failed to selectTable %v", err)
	}
	if len(dbRet) == 0 {
		return nil, fmt.Errorf("no selected data")
	}

	var dateCloses []dateClose
	// 日付と終値の２つを取得
	for i := 0; i < len(dbRet); i += 2 {
		// float64型数値に変換
		// 株価には小数点が入っていることがあるのでfloatで扱う
		c, err := strconv.ParseFloat(dbRet[i+1], 64)
		if err != nil {
			return nil, fmt.Errorf("failed to ParseFloat. %v", err)
		}
		dateCloses = append(dateCloses, dateClose{Date: dbRet[i], Close: c})
		//log.Infof(ctx, "dbRet[i] %s dbRet[i+1] %s", dbRet[i], dbRet[i+1])
	}
	//log.Infof(ctx, "dateCloses %v", dateCloses)
	return dateCloses, nil
}

// 銘柄コード、必要な移動平均、日付を渡すと該当のX日移動平均を返す
func getMoving(r *http.Request, db *sql.DB, code string, movingDay string, date string) (float64, error) {
	//ctx := appengine.NewContext(r)

	dbRet, err := selectTable(r, db, fmt.Sprintf(
		"SELECT %s FROM movingavg WHERE code = %s and date = '%s';", movingDay, code, date))
	if err != nil {
		return 0.0, fmt.Errorf("failed to selectTable %v", err)
	}
	if len(dbRet) == 0 {
		return 0.0, fmt.Errorf("no selected data")
	}

	// []interface {}型のdbRetをfloat64に変換
	moving, _ := strconv.ParseFloat(dbRet[0], 64)

	//log.Infof(ctx, "%f", moving)
	return moving, nil
}

// 銘柄コード、日付を渡すと該当のmovings structに対応するX日移動平均を返す
// TODO：ベタ書きではなくreflectを使ってmovingsの項目が増えても対応できるようにしたい
func getMovings(r *http.Request, db *sql.DB, code string, date string) (movings, error) {
	//ctx := appengine.NewContext(r)

	movingDays := []string{"moving5", "moving20", "moving60", "moving100"}
	ms, err := selectTable(r, db, fmt.Sprintf(
		"SELECT %s FROM movingavg WHERE code = %s and date = '%s';", strings.Join(movingDays, ","), code, date))
	if err != nil {
		return movings{}, fmt.Errorf("failed to selectTable %v", err)
	}
	if len(ms) == 0 {
		return movings{}, fmt.Errorf("no selected data")
	}

	var mf []float64
	for _, m := range ms {
		// []interface型のmsをfloat64に変換
		f, err := strconv.ParseFloat(m, 64)
		if err != nil {
			return movings{}, fmt.Errorf("failed to ParseFloat %v", err)
		}
		mf = append(mf, f)
	}
	return movings{mf[0], mf[1], mf[2], mf[3]}, nil
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
		dbRet, err := selectTable(r, db, query)
		if err != nil {
			log.Errorf(ctx, "failed to selectTable %v", err)
			os.Exit(0)
		}
		log.Infof(ctx, "fetched %d codes from 'daily' in db. target date: %s", len(dbRet), previousBussinessDay)

		// あとで全銘柄と比較するためにmapに格納
		dbCodesMap := map[int]bool{}
		for _, v := range dbRet {
			code, _ := strconv.Atoi(v)
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
	log.Infof(ctx, "done ensureDailyDBHandler.")
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
