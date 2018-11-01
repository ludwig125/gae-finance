package main

import (
	"fmt"
	"io/ioutil"
	//"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/appengine" // Required external App Engine library
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch" // 外部にhttpするため
)

func main() {
	http.HandleFunc("/_ah/start", start)
	http.HandleFunc("/daily", indexHandlerDaily)
	http.HandleFunc("/", indexHandler)
	appengine.Main() // Starts the server to receive requests
}

func start(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	log.Infof(c, "STARTING")
}

// codeごとの株価
type code_price struct {
	Code  string
	Price [][]string
}

// Dailyの株価を取得
func indexHandlerDaily(w http.ResponseWriter, r *http.Request) {

	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)

	// GAE log
	ctx := appengine.NewContext(r)

	// spreadsheets clientを取得
	sheetService, err := sheets.New(client)
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve Sheets Client %v", err)
	}

	// spreadsheetから銘柄コードを取得
	codes := readCode(sheetService, r, "ichibu")

	//fmt.Fprintln(w, codes)
	if len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		os.Exit(0)
	}
	MAX := 100
	d := len(codes) / MAX
	m := len(codes) % MAX

	if d > 0 {
		for i := 0; i < d; i++ {
			partial := codes[MAX*i : MAX*(i+1)]
			//log.Warningf(ctx, "partial %v, type %T\n", partial, partial)
			// MAX単位でcodeをScrapeしてSpreadSheetに書き込み
			processPartialCode(partial, sheetService, r)
			//err := processPartialCode(partial, sheetService, r)
			//if err != nil {
			//	log.Warningf(ctx, "%v\n", err)
			//}
		}
	}
	if m > 0 {
		partial := codes[MAX*d:]
		// MAX単位でcodeをScrapeしてSpreadSheetに書き込み
		processPartialCode(partial, sheetService, r)
		//err := processPartialCode(partial, sheetService, r)
		//if err != nil {
		//	log.Warningf(ctx, "%v\n", err)
		//}

	}

}

func processPartialCode(codes [][]interface{}, s *sheets.Service, r *http.Request) {
	ctx := appengine.NewContext(r)

	var prices []code_price

	var allErrors string
	for _, v := range codes {
		code := v[0].(string) // row's type: []interface {}. ex. [8411]

		// codeごとに株価を取得
		p, err := doScrapeDaily(r, code)
		if err != nil {
			//log.Infof(ctx, "err: %v", err)
			allErrors += fmt.Sprintf("%v ", err)
			continue
		}
		prices = append(prices, code_price{code, p})
		//log.Infof(ctx, "code: %s, prices: %v", code, prices)
		time.Sleep(1 * time.Second) // 1秒待つ
	}
	if allErrors != "" {
		//return fmt.Errorf("failed to scrape. code: [%s]", allErrors)
		// 複数の銘柄で起きたエラーをまとめて出力
		log.Warningf(ctx, "failed to scrape. code: [%s]\n", allErrors)
	}
	//log.Warningf(ctx, "prices. %v", prices)
	writeStockpriceDaily(s, r, prices)

	return
}

// codeごとの株価比率
type code_rate struct {
	Code string
	Rate []float64
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// googleAPIへのclientをリクエストから作成
	client := getClientWithJson(r)

	// GAE log
	ctx := appengine.NewContext(r)

	// spreadsheets clientを取得
	sheetService, err := sheets.New(client)
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve Sheets Client %v", err)
	}

	if !isBussinessday(sheetService, r) {
		log.Infof(ctx, "Is not a business day today.")
		return
	}

	// spreadsheetから銘柄コードを取得
	codes := readCode(sheetService, r, "code")

	//fmt.Fprintln(w, codes)

	if len(codes) == 0 {
		log.Infof(ctx, "No target data.")
	} else {
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
		resp := getSheetData(r, sheetService, "STOCKPRICE_SHEETID", "stockprice")

		// 全codeの株価比率
		var whole_code_rate []code_rate
		for _, row := range codes {
			code := row[0].(string)
			rate, err := calcIncreaseRate(resp, code)
			if err != nil {
				log.Warningf(ctx, "%v\n", err)
				continue
			}
			whole_code_rate = append(whole_code_rate, code_rate{code, rate})
		}
		log.Infof(ctx, "count whole code %v\n", len(whole_code_rate))

		// 一つ前との比率が一番大きいもの順にソート
		sort.SliceStable(whole_code_rate, func(i, j int) bool { return whole_code_rate[i].Rate[0] > whole_code_rate[j].Rate[0] })
		fmt.Fprintln(w, whole_code_rate)

		// 事前にrateのシートをclear
		clearRate(sheetService, r)

		// 株価の比率順にソートしたものを書き込み
		writeRate(sheetService, r, whole_code_rate)
	}
}

func getClientWithJson(r *http.Request) *http.Client {
	// リクエストからcontextを作成
	// GAE log
	ctx := appengine.NewContext(r)

	credentialFilePath := "myfinance-01-dc1116b8f354.json"
	data, err := ioutil.ReadFile(credentialFilePath)
	if err != nil {
		log.Errorf(ctx, "Unable to read client secret file: %v", err)
	}
	conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		log.Errorf(ctx, "Unable to parse client secret file to config: %v", err)
	}
	return conf.Client(ctx)
}

func isBussinessday(srv *sheets.Service, r *http.Request) bool {
	ctx := appengine.NewContext(r)

	if v := os.Getenv("ENV"); v == "test" {
		// test環境は常にtrue
		return true
	} else if v == "prod" {

		// 土日は実行しない
		jst, _ := time.LoadLocation("Asia/Tokyo")
		now := time.Now().In(jst)

		d := now.Weekday()
		switch d {
		case 6, 0: // Saturday, Sunday
			return false
		}

		today_ymd := now.Format("2006/01/02")

		holidays := getHolidays(srv, r)
		for _, row := range holidays {
			holiday := row[0].(string)
			if holiday == today_ymd {
				log.Infof(ctx, "Today is holiday.")
				return false
			}
		}
	} else {
		// ENVがprodでもtestでもない場合は異常終了
		log.Errorf(ctx, "ENV must be 'test' or 'prod': %v", v)
		os.Exit(0)
	}
	return true

}

func getHolidays(srv *sheets.Service, r *http.Request) [][]interface{} {
	ctx := appengine.NewContext(r)

	// 'holiday' worksheet を読み取り
	// 東京証券取引所の休日: https://www.jpx.co.jp/corporate/calendar/index.html

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("HOLIDAY_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get holiday sheetId. '%v'", v)
		os.Exit(0)
	}
	readRange := "holiday"
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	return resp.Values
}

func readCode(srv *sheets.Service, r *http.Request, sheet string) [][]interface{} {
	ctx := appengine.NewContext(r)

	// 'code' worksheet を読み取り

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("CODE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get codes sheetId. '%v'", v)
		os.Exit(0)
	}
	//readRange := "code"
	readRange := sheet
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	return resp.Values
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
	//ctx := appengine.NewContext(r)
	//log.Errorf(ctx, "len date_price %d", len(date_price[0]))
	if len(date_price[0]) != 7 {
		// 以下の７要素を取れなかったら何かおかしい
		// 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値
		// リダイレクトされて別のページに飛ばされている可能性もある
		// 失敗した銘柄を返す
		return nil, fmt.Errorf(code)
	}
	return date_price, nil
}

func doScrape(r *http.Request, code string) (string, string, error) {

	// "HOURLY_PRICE_URL"のHDML doc取得
	doc, err := fetchWebpageDoc(r, "HOURLY_PRICE_URL", code)
	if err != nil {
		return "", "", err
	}
	//v3 := reflect.ValueOf(doc)
	//ctx := appengine.NewContext(r)
	//log.Debugf(ctx, "doc type: %v", v3.Type())
	//log.Debugf(ctx, "doc: %v", doc)

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
	//return getFormatedDate(time, r), getFormatedPrice(price, r)
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

func writeStockpriceDaily(srv *sheets.Service, r *http.Request, prices []code_price) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "daily"

	//log.Debugf(ctx, "%s, %s", sheetId, writeRange)

	// spreadsheetに書き込み対象の行列を作成
	var matrix = make([][]interface{}, 0)
	for _, p := range prices {
		for _, daily_p := range p.Price {
			var ele = make([]interface{}, 0)
			ele = append(ele, p.Code)
			for _, v := range daily_p {
				ele = append(ele, v)
			}
			// ele ex. [8306 8/23 320 322 317 319 8068000 319.0]
			matrix = append(matrix, ele)
		}
	}
	//log.Errorf(ctx, "code prices %v", matrix)
	//log.Errorf(ctx, "code prices %T", matrix)
	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		//matrix : [][]interface{} 型
		// [銘柄, 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値] * 日付
		Values: matrix,
	}

	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

//func writeStockpriceDaily(srv *sheets.Service, r *http.Request, code string, dp []string) {
//	ctx := appengine.NewContext(r)
//
//	sheetId := ""
//	// sheetIdを環境変数から読み込む
//	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
//		sheetId = v
//	} else {
//		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
//		os.Exit(0)
//	}
//	writeRange := "daily"
//
//	valueRange := &sheets.ValueRange{
//		MajorDimension: "ROWS",
//		Values: [][]interface{}{
//			// code, 日付, 始値, 高値, 安値, 終値, 売買高, 修正後終値
//			[]interface{}{code, dp[0], dp[1], dp[2], dp[3], dp[4], dp[5], dp[6]},
//		},
//	}
//	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
//	if err != nil {
//		log.Errorf(ctx, "Unable to write value. %v", err)
//	}
//	status := resp.ServerResponse.HTTPStatusCode
//	if status != 200 {
//		log.Errorf(ctx, "HTTPstatus error. %v", status)
//	}
//}

func writeStockprice(srv *sheets.Service, r *http.Request, code string, date string, stockprice string) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("STOCKPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "stockprice"

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values: [][]interface{}{
			[]interface{}{code, date, stockprice},
		},
	}
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

func getSheetData(r *http.Request, srv *sheets.Service, sid string, sname string) [][]interface{} {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv(sid); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	readRange := sname
	// stockpriceシートからデータを取得
	resp, err := srv.Spreadsheets.Values.Get(sheetId, readRange).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to retrieve data from sheet: %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
	v := reflect.ValueOf(resp)
	log.Debugf(ctx, "type: %v", v.Type())
	log.Debugf(ctx, resp.Range)
	log.Debugf(ctx, "len resp.Values: %v", len(resp.Values)) //これでデータの全行数が取れる

	//v1 := resp.Values[0]
	//log.Debugf(ctx, "v1: %v, v1[0]: %v, v1[1]: %v", v1, v1[0], v1[1])
	return resp.Values
}

func calcIncreaseRate(resp [][]interface{}, code string) ([]float64, error) {

	//v2 := resp[0]
	//log.Debugf(ctx, "v2: %v, v2[0]: %v, v2[1]: %v, v2[2]: %v", v2, v2[0], v2[1], v2[2])
	DATA_NUM := 7

	var price []float64
	// 後ろから順番に読んでいく
	count := DATA_NUM
	for i := 0; i < len(resp); i++ {
		v := resp[len(resp)-1-i] // [8316 2018/08/09 15:00 4426]
		//		log.Debugf(ctx, "v: %v, v[0]: %v, v[1]: %v", v, v[0], v[1])
		if len(v) < 3 {
			return nil, fmt.Errorf("code %s record is unsatisfied. record: %v", code, v)
		}
		if v[0] == code {
			p, err := strconv.ParseFloat(v[2].(string), 64)
			if err != nil {
				return nil, fmt.Errorf("code %s's price cannot be converted to ParseFloat. record: %v. err: %v", code, v, err)
			}
			//log.Debugf(ctx, "p: %v", p)

			price = append(price, p)
			//va := reflect.ValueOf(v[2].(string)) // 型確認
			//log.Println(va.Type(), v[2])

			count = count - 1
			// 指定回数分取得したらループを抜ける
			if count <= 0 {
				break
			}
		}
	}
	// price ex. [685.1 684.6 683.2 684.2 686.9 684.3 684.3]
	//log.Println(code, price)

	// rate[0] = price[0]/price[1]
	// ...
	// rate[DATA_NUM-2] = price[DATA_NUM-2]/price[DATA_NUM-1]
	var rate []float64
	for i := 0; i < DATA_NUM-1; i++ {
		if i+1 < len(price) {
			rate = append(rate, price[0]/price[i+1])
		} else {
			rate = append(rate, 0.0)
		}
	}
	//log.Println(code, rate)
	// rate ex. [1.0007303534910896 1.002781030444965 1.0013154048523822 0.997379531227253 1.0011690778898146 1.0011690778898146]
	return rate, nil
}

func clearRate(srv *sheets.Service, r *http.Request) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("RATE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "rate"

	// clear stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Clear(sheetId, writeRange, &sheets.ClearValuesRequest{}).Do()
	if err != nil {
		log.Errorf(ctx, "Unable to clear value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}

func writeRate(srv *sheets.Service, r *http.Request, rate []code_rate) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("RATE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "rate"

	// spreadsheetに書き込み対象の行列を作成
	matrix := make([][]interface{}, len(rate))
	// 株価の比率順にソートしたものを書き込み
	for i, r := range rate {
		matrix[i] = []interface{}{r.Code, r.Rate[0], r.Rate[1], r.Rate[2], r.Rate[3], r.Rate[4], r.Rate[5]}
	}

	valueRange := &sheets.ValueRange{
		MajorDimension: "ROWS",
		Values:         matrix,
	}
	// Write stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
	if err != nil {
		log.Errorf(ctx, "Unable to write value. %v", err)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
	}
}
