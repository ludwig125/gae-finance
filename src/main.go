package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
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

// codeごとの株価
type codePrice struct {
	Code  string
	Price []string
}

// codeごとの株価比率
type codeRate struct {
	Code string
	Rate []float64
}

func main() {
	http.HandleFunc("/_ah/start", start)
	http.HandleFunc("/daily", indexHandlerDaily)
	http.HandleFunc("/calc_daily", indexHandlerCalcDaily)
	http.HandleFunc("/", indexHandler)
	appengine.Main() // Starts the server to receive requests
}

// バッチ処理のbasic_scalingを使うために /_ah/startのハンドラが必要
func start(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	log.Infof(c, "STARTING")
}

func indexHandlerDaily(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

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
	codes := readCode(sheetService, r, "ichibu")
	if len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		os.Exit(0)
	}

	// 100件ずつスクレイピングしてSheetに書き込み
	MAX := 100
	d := len(codes) / MAX
	m := len(codes) % MAX

	if d > 0 {
		for i := 0; i < d; i++ {
			partial := codes[MAX*i : MAX*(i+1)]
			// MAX単位でcodeをScrapeしてSpreadSheetに書き込み
			processPartialCode(partial, sheetService, r)
		}
	}
	if m > 0 {
		partial := codes[MAX*d:]
		// MAX単位でcodeをScrapeしてSpreadSheetに書き込み
		processPartialCode(partial, sheetService, r)
	}

}

func processPartialCode(codes [][]interface{}, s *sheets.Service, r *http.Request) {
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
	// spreadsheetから株価を取得する
	resp := getSheetData(r, s, "DAILYPRICE_SHEETID", "daily")
	if resp == nil {
		return
	}

	// シートに存在しないものだけを抽出
	uniqPrices := getUniqPrice(r, prices, resp)
	// すでにシートに存在するデータは書き込まない
	if uniqPrices != nil {
		writeStockpriceDaily(s, r, uniqPrices)
	}

	return
}

func indexHandlerCalcDaily(w http.ResponseWriter, r *http.Request) {
	// GAE log
	ctx := appengine.NewContext(r)

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
	codes := readCode(sheetService, r, "ichibu")
	if len(codes) == 0 {
		log.Infof(ctx, "No target data.")
		return
	}

	// spreadsheetから株価を取得する
	resp := getSheetData(r, sheetService, "DAILYPRICE_SHEETID", "daily")
	if resp == nil {
		log.Infof(ctx, "No data")
		return
	}

	cdmp := codeDateModprice(r, resp)
	//log.Infof(ctx, "%v\n", cdmp)

	// 全codeの株価比率
	var whole_codeRate []codeRate
	for _, row := range codes {
		code := row[0].(string)
		//直近7日間の増減率を取得する
		rate, err := calcIncreaseRate(cdmp, code, 7, r)
		if err != nil {
			log.Warningf(ctx, "%v\n", err)
			continue
		}
		whole_codeRate = append(whole_codeRate, codeRate{code, rate})
	}
	log.Infof(ctx, "count whole code %v\n", len(whole_codeRate))

	// 一つ前との比率が一番大きいもの順にソート
	sort.SliceStable(whole_codeRate, func(i, j int) bool { return whole_codeRate[i].Rate[0] > whole_codeRate[j].Rate[0] })
	//fmt.Fprintln(w, whole_codeRate)

	// 事前にrateのシートをclear
	clearSheet(sheetService, r, "DAILYRATE_SHEETID", "daily_rate")

	// 株価の比率順にソートしたものを書き込み
	writeRate(sheetService, r, whole_codeRate, "DAILYRATE_SHEETID", "daily_rate")
}

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
	codes := readCode(sheetService, r, "code")
	if len(codes) == 0 {
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
	resp := getSheetData(r, sheetService, "STOCKPRICE_SHEETID", "stockprice")
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
	clearSheet(sheetService, r, "RATE_SHEETID", "rate")

	// 株価の比率順にソートしたものを書き込み
	writeRate(sheetService, r, whole_codeRate, "RATE_SHEETID", "rate")
}

func getClientWithJson(r *http.Request) *http.Client {
	// リクエストからcontextを作成
	// GAE log
	ctx := appengine.NewContext(r)

	credentialFilePath := "myfinance-01-dc1116b8f354.json"
	data, err := ioutil.ReadFile(credentialFilePath)
	if err != nil {
		log.Errorf(ctx, "Unable to read client secret file: %v", err)
		os.Exit(0)
	}
	conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		log.Errorf(ctx, "Unable to parse client secret file to config: %v", err)
		os.Exit(0)
	}
	return conf.Client(ctx)
}

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
		os.Exit(0)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
		os.Exit(0)
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
		os.Exit(0)
	}
	status := resp.ServerResponse.HTTPStatusCode
	if status != 200 {
		log.Errorf(ctx, "HTTPstatus error. %v", status)
		os.Exit(0)
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

func getUniqPrice(r *http.Request, prices []codePrice, resp [][]interface{}) []codePrice {
	ctx := appengine.NewContext(r)

	sheetData := map[string]bool{}
	for _, v := range resp {
		// codeと日付の組をmapに登録
		cd := fmt.Sprintf("%s %s", v[0], v[1])
		sheetData[cd] = true
		//log.Errorf(ctx, "code date %v", d)
	}

	var uniqPrices []codePrice
	for _, p := range prices {
		// codeと日付の組をmapに登録済みのデータと照合
		cd := fmt.Sprintf("%s %s", p.Code, p.Price[0])
		if !sheetData[cd] {
			uniqPrices = append(uniqPrices, codePrice{p.Code, p.Price})
			//log.Debugf(ctx, "uniq code price %s %v", p.Code, p.Price)
		} else {
			log.Debugf(ctx, "duplicated code price %s %v", p.Code, p.Price)
		}
	}
	return uniqPrices
}

func writeStockpriceDaily(srv *sheets.Service, r *http.Request, prices []codePrice) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv("DAILYPRICE_SHEETID"); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get stockprice sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := "daily"

	// spreadsheetに書き込み対象の行列を作成
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

	var MaxRetries = 3
	attempt := 0
	for {
		// MaxRetries を超えていたら終了
		if attempt >= MaxRetries {
			log.Errorf(ctx, "Failed to retrieve data from sheet. attempt: %d", attempt)
			return
		}
		attempt = attempt + 1
		resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
		if err != nil {
			log.Warningf(ctx, "Unable to write value. %v. attempt: %d", err, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		status := resp.ServerResponse.HTTPStatusCode
		if status != 200 {
			log.Warningf(ctx, "HTTPstatus error. %v. attempt: %d", status, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		return
	}
}

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
	var MaxRetries = 3
	attempt := 0
	for {
		// MaxRetries を超えていたら終了
		if attempt >= MaxRetries {
			log.Errorf(ctx, "Failed to retrieve data from sheet. attempt: %d", attempt)
			return
		}
		attempt = attempt + 1
		resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
		if err != nil {
			log.Warningf(ctx, "Unable to write value. %v. attempt: %d", err, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		status := resp.ServerResponse.HTTPStatusCode
		if status != 200 {
			log.Warningf(ctx, "HTTPstatus error. %v. attempt: %d", status, attempt)
			time.Sleep(1 * time.Second) // 1秒待つ
			continue
		}
		return
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

	var MaxRetries = 3
	attempt := 0
	for {
		// MaxRetries を超えていたらnilを返す
		if attempt >= MaxRetries {
			log.Errorf(ctx, "Failed to retrieve data from sheet. attempt: %d", attempt)
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

func clearSheet(srv *sheets.Service, r *http.Request, sid string, sname string) {
	ctx := appengine.NewContext(r)

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv(sid); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := sname

	// clear stockprice rate spreadsheet:
	resp, err := srv.Spreadsheets.Values.Clear(sheetId, writeRange, &sheets.ClearValuesRequest{}).Do()
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

	sheetId := ""
	// sheetIdを環境変数から読み込む
	if v := os.Getenv(sid); v != "" {
		sheetId = v
	} else {
		log.Errorf(ctx, "Failed to get price rate sheetId. '%v'", v)
		os.Exit(0)
	}
	writeRange := sname

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
	resp, err := srv.Spreadsheets.Values.Append(sheetId, writeRange, valueRange).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Do()
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
