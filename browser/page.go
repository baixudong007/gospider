package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	uurl "net/url"
	"os"
	"time"

	"gitee.com/baixudong/gospider/bs4"
	"gitee.com/baixudong/gospider/cdp"
	"gitee.com/baixudong/gospider/re"
	"gitee.com/baixudong/gospider/requests"
	"gitee.com/baixudong/gospider/tools"

	"github.com/tidwall/gjson"
)

type Page struct {
	host       string
	port       int
	id         string
	mouseX     float64
	mouseY     float64
	ctx        context.Context
	cnl        context.CancelFunc
	preWebSock *cdp.WebSock
	ReqCli     *requests.Client
	isMove     bool

	nodeId  int64
	baseUrl string
	webSock *cdp.WebSock
}
type PageOption struct {
	Proxy    string //代理
	GetProxy func() (string, error)
}

func (obj *Page) init(proxy string, getProxy func() (string, error), db *cdp.DbClient) error {
	var err error
	if obj.webSock, err = cdp.NewWebSock(
		obj.ctx,
		fmt.Sprintf("ws://%s:%d/devtools/page/%s", obj.host, obj.port, obj.id),
		fmt.Sprintf("http://%s:%d/", obj.host, obj.port),
		proxy,
		getProxy,
		db,
	); err != nil {
		return err
	}
	obj.ReqCli, err = requests.NewClient(obj.ctx)
	if err != nil {
		return err
	}
	if _, err = obj.webSock.PageEnable(obj.ctx); err != nil {
		return err
	}
	if err = obj.AddScript(obj.ctx, stealth); err != nil {
		return err
	}
	return obj.AddScript(obj.ctx, stealth2)
}
func (obj *Page) AddScript(ctx context.Context, script string) error {
	_, err := obj.webSock.PageAddScriptToEvaluateOnNewDocument(ctx, script)
	return err
}
func (obj *Page) Png(ctx context.Context, path string) error {
	rect, err := obj.GetLayoutMetrics(ctx)
	if err != nil {
		return err
	}
	rs, err := obj.webSock.PageCaptureScreenshot(ctx, rect)
	if err != nil {
		return err
	}
	imgData, ok := rs.Result["data"].(string)
	if !ok {
		return errors.New("not img data")
	}
	imgCon, err := tools.Base64Decode(imgData)
	if err != nil {
		return err
	}
	return os.WriteFile(path, imgCon, 0777)
}

func (obj *Page) GetLayoutMetrics(ctx context.Context) (cdp.LayoutMetrics, error) {
	rs, err := obj.webSock.PageGetLayoutMetrics(ctx)
	var result cdp.LayoutMetrics
	if err != nil {
		return result, err
	}
	return result, tools.Map2struct(rs.Result, &result)
}
func (obj *Page) Reload(ctx context.Context) error {
	_, err := obj.webSock.PageReload(ctx)
	return err
}
func (obj *Page) GoTo(preCtx context.Context, url string) error {
	var err error
	obj.baseUrl = url
	var ctx context.Context
	var cnl context.CancelFunc
	if preCtx == nil {
		ctx, cnl = context.WithTimeout(obj.ctx, time.Second*30)
		defer cnl()
	} else {
		ctx = preCtx
	}
	_, err = obj.webSock.PageNavigate(ctx, url)
	if err != nil {
		return err
	}
	methodEvent := obj.webSock.RegMethod(ctx, "Page.frameStartedLoading", "Page.frameStoppedLoading")
	defer methodEvent.Cnl()
waitLoading:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-obj.Done():
			return errors.New("websocks closed")
		case methodRecvData := <-methodEvent.RecvData:
			if methodRecvData.Method == "Page.frameStoppedLoading" {
				methodId := methodRecvData.Id
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-obj.Done():
						return errors.New("websocks closed")
					case methodRecvData := <-methodEvent.RecvData:
						if methodRecvData.Method == "Page.frameStartedLoading" && methodRecvData.Id > methodId {
							goto waitLoading
						}
					case <-time.After(time.Millisecond * 1500):
						return nil
					}
				}
			}
		}
	}
}

// ex:   ()=>{}  或者  (params)=>{}
func (obj *Page) Eval(ctx context.Context, expression string, params map[string]any) (gjson.Result, error) {
	var value string
	if params != nil {
		con, err := json.Marshal(params)
		if err != nil {
			return gjson.Result{}, err
		}
		value = tools.BytesToString(con)
	}
	// log.Print(fmt.Sprintf(`(async %s)(%s)`, expression, value))
	rs, err := obj.webSock.RuntimeEvaluate(ctx, fmt.Sprintf(`(async %s)(%s)`, expression, value))
	return tools.Any2json(rs.Result), err
}
func (obj *Page) Close() error {
	defer obj.cnl()
	_, err := obj.preWebSock.TargetCloseTarget(obj.id)
	if err != nil {
		err = obj.close()
	}
	obj.webSock.Close()
	return err
}
func (obj *Page) close() error {
	resp, err := obj.ReqCli.Request(context.TODO(), "get", fmt.Sprintf("http://%s:%d/json/close/%s", obj.host, obj.port, obj.id), requests.RequestOption{DisProxy: true})
	if err != nil {
		return err
	}
	if resp.Text() == "Target is closing" {
		return nil
	}
	return errors.New(resp.Text())
}

func (obj *Page) Done() <-chan struct{} {
	return obj.webSock.Done()
}
func (obj *Page) Route(ctx context.Context, routeFunc func(context.Context, *cdp.Route)) error {
	obj.webSock.RouteFunc = routeFunc
	var err error
	if obj.webSock.RouteFunc != nil {
		_, err = obj.webSock.FetchEnable(ctx)
	} else {
		_, err = obj.webSock.FetchDisable(ctx)
	}
	return err
}
func (obj *Page) initNodeId(ctx context.Context) error {
	rs, err := obj.webSock.DOMGetDocument(ctx)
	if err != nil {
		return err
	}
	jsonData := tools.Any2json(rs.Result["root"])
	href := jsonData.Get("baseURL").String()
	if href != "" {
		obj.baseUrl = href
	}
	obj.nodeId = jsonData.Get("nodeId").Int()
	return nil
}
func (obj *Page) Html(ctx context.Context, contents ...string) (*bs4.Client, error) {
	err := obj.initNodeId(ctx)
	if err != nil {
		return nil, err
	}
	if len(contents) > 0 {
		return nil, obj.setHtml(ctx, contents[0])
	}
	return obj.html(ctx)
}
func (obj *Page) setHtml(ctx context.Context, content string) error {
	_, err := obj.webSock.DOMSetOuterHTML(ctx, obj.nodeId, content)
	return err
}
func (obj *Page) html(ctx context.Context) (*bs4.Client, error) {
	rs, err := obj.webSock.DOMGetOuterHTML(ctx, obj.nodeId)
	if err != nil {
		return nil, err
	}
	html := bs4.NewClient(rs.Result["outerHTML"].(string), obj.baseUrl)
	iframes := html.Finds("iframe")
	if len(iframes) > 0 {
		pageFrams, err := obj.QuerySelectorAll(ctx, "iframe")
		if err != nil {
			return nil, err
		}
		if len(iframes) != len(pageFrams) {
			return nil, errors.New("iframe error")
		}
		for i, ifram := range iframes {
			dh, err := pageFrams[i].Html(ctx)
			if err != nil {
				return nil, err
			}
			ifram.Html(dh.Html())
		}
	}
	return html, nil
}
func (obj *Page) WaitSelector(preCtx context.Context, selector string, timeouts ...int64) (*Dom, error) {
	for {
		dom, err := obj.QuerySelector(preCtx, selector)
		if err != nil {
			return nil, err
		}
		if dom != nil {
			return dom, nil
		}
		time.Sleep(time.Millisecond * 500)
	}
}
func (obj *Page) QuerySelector(ctx context.Context, selector string) (*Dom, error) {
	err := obj.initNodeId(ctx)
	if err != nil {
		return nil, err
	}
	dom, err := obj.querySelector(ctx, selector)
	if err != nil {
		return dom, err
	}
	if dom == nil && selector != "iframe" {
		iframes, err := obj.querySelectorAll(ctx, "iframe")
		if err != nil {
			return nil, err
		}
		for _, iframe := range iframes {
			dom, err = iframe.querySelector(ctx, selector)
			if err != nil || dom != nil {
				return dom, err
			}
		}
	}
	return dom, err
}
func (obj *Page) querySelector(ctx context.Context, selector string) (*Dom, error) {
	rs, err := obj.webSock.DOMQuerySelector(ctx, obj.nodeId, selector)
	if err != nil {
		return nil, err
	}
	nodeId := int64(rs.Result["nodeId"].(float64))
	if nodeId == 0 {
		return nil, nil
	}
	dom := &Dom{
		baseUrl: obj.baseUrl,
		webSock: obj.webSock,
		nodeId:  nodeId,
	}
	if re.Search(`^iframe\W|\Wiframe\W|\Wiframe$|^iframe$`, selector) != nil {
		if err = dom.dom2Iframe(ctx); err != nil {
			return nil, err
		}
	}
	return dom, nil
}
func (obj *Page) QuerySelectorAll(ctx context.Context, selector string) ([]*Dom, error) {
	err := obj.initNodeId(ctx)
	if err != nil {
		return nil, err
	}
	dom, err := obj.querySelectorAll(ctx, selector)
	if err != nil {
		return dom, err
	}
	if dom == nil && selector != "iframe" {
		iframes, err := obj.querySelectorAll(ctx, "iframe")
		if err != nil {
			return nil, err
		}
		doms := []*Dom{}
		for _, iframe := range iframes {
			dom, err = iframe.querySelectorAll(ctx, selector)
			if err != nil {
				return dom, err
			}
			doms = append(doms, dom...)
		}
		return doms, err
	}
	return dom, err
}
func (obj *Page) querySelectorAll(ctx context.Context, selector string) ([]*Dom, error) {
	rs, err := obj.webSock.DOMQuerySelectorAll(ctx, obj.nodeId, selector)
	if err != nil {
		return nil, err
	}
	doms := []*Dom{}
	for _, nodeId := range tools.Any2json(rs.Result["nodeIds"]).Array() {
		dom := &Dom{
			baseUrl: obj.baseUrl,
			webSock: obj.webSock,
			nodeId:  nodeId.Int(),
		}
		if re.Search(`^iframe\W|\Wiframe\W|\Wiframe$|^iframe$`, selector) != nil {
			if err = dom.dom2Iframe(ctx); err != nil {
				return nil, err
			}
		}
		doms = append(doms, dom)
	}
	return doms, nil
}
func (obj *Page) baseMove(ctx context.Context, point cdp.Point, kind int, steps ...int) error {
	if !obj.isMove {
		obj.mouseX = point.X
		obj.mouseY = point.Y
		obj.isMove = true
		return nil
	}
	var step int
	if len(steps) > 0 {
		step = steps[0]
	}
	if step < 1 {
		step = 1
	}
	for _, poi := range tools.GetTrack(
		[2]float64{obj.mouseX, obj.mouseY},
		[2]float64{point.X, point.Y},
		float64(step),
	) {
		switch kind {
		case 0:
			if err := obj.move(ctx, cdp.Point{
				X: poi[0],
				Y: poi[1],
			}); err != nil {
				return err
			}
		case 1:
			if err := obj.touchMove(ctx, cdp.Point{
				X: poi[0],
				Y: poi[1],
			}); err != nil {
				return err
			}
		default:
			return errors.New("not found kind")
		}
	}
	obj.isMove = true
	return nil
}

func (obj *Page) Move(ctx context.Context, point cdp.Point, steps ...int) error {
	return obj.baseMove(ctx, point, 0, steps...)
}
func (obj *Page) move(ctx context.Context, point cdp.Point) error {
	_, err := obj.webSock.InputDispatchMouseEvent(ctx,
		cdp.DispatchMouseEventOption{
			Type: "mouseMoved",
			X:    point.X,
			Y:    point.Y,
		})
	if err != nil {
		return err
	}
	obj.mouseX = point.X
	obj.mouseY = point.Y
	return nil
}

func (obj *Page) Down(ctx context.Context, point cdp.Point) error {
	_, err := obj.webSock.InputDispatchMouseEvent(ctx,
		cdp.DispatchMouseEventOption{
			Type:   "mousePressed",
			Button: "left",
			X:      point.X,
			Y:      point.Y,
		})
	if err != nil {
		return err
	}
	obj.mouseX = point.X
	obj.mouseY = point.Y
	obj.isMove = true
	return err
}
func (obj *Page) Up(ctx context.Context) error {
	_, err := obj.webSock.InputDispatchMouseEvent(ctx, cdp.DispatchMouseEventOption{
		Type:   "mouseReleased",
		Button: "left",
		X:      obj.mouseX,
		Y:      obj.mouseY,
	})
	return err
}
func (obj *Page) Click(ctx context.Context, point cdp.Point) error {
	if err := obj.Down(ctx, point); err != nil {
		return err
	}
	return obj.Up(ctx)
}

func (obj *Page) TouchMove(ctx context.Context, point cdp.Point, steps ...int) error {
	return obj.baseMove(ctx, point, 1, steps...)
}
func (obj *Page) touchMove(ctx context.Context, point cdp.Point) error {
	_, err := obj.webSock.InputDispatchTouchEvent(ctx, "touchMove", []cdp.Point{
		{
			X: point.X,
			Y: point.Y,
		},
	})
	if err != nil {
		return err
	}
	obj.mouseX = point.X
	obj.mouseY = point.Y
	return nil
}
func (obj *Page) TouchDown(ctx context.Context, point cdp.Point) error {
	_, err := obj.webSock.InputDispatchTouchEvent(ctx, "touchStart",
		[]cdp.Point{
			point,
		})
	if err != nil {
		return err
	}
	obj.mouseX = point.X
	obj.mouseY = point.Y
	return nil
}
func (obj *Page) TouchUp(ctx context.Context) error {
	_, err := obj.webSock.InputDispatchTouchEvent(ctx,
		"touchEnd",
		[]cdp.Point{})
	return err
}

// 设置移动设备的属性
func (obj *Page) SetDevice(ctx context.Context, device cdp.Device) error {
	if err := obj.SetUserAgent(ctx, device.UserAgent); err != nil {
		return err
	}
	if err := obj.SetTouch(ctx, device.HasTouch); err != nil {
		return err
	}
	return obj.SetDeviceMetrics(ctx, device)
}

func (obj *Page) SetUserAgent(ctx context.Context, userAgent string) error {
	_, err := obj.webSock.EmulationSetUserAgentOverride(ctx, userAgent)
	return err
}

// 设置设备指标
func (obj *Page) SetDeviceMetrics(ctx context.Context, device cdp.Device) error {
	_, err := obj.webSock.EmulationSetDeviceMetricsOverride(ctx, device)
	return err
}

// 设置设备是否支持触摸
func (obj *Page) SetTouch(ctx context.Context, hasTouch bool) error {
	_, err := obj.webSock.EmulationSetTouchEmulationEnabled(ctx, hasTouch)
	return err
}

func (obj *Page) SetCookies(ctx context.Context, cookies ...cdp.Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	var err error
	for i := 0; i < len(cookies); i++ {
		if cookies[i].Domain == "" {
			if cookies[i].Url == "" {
				cookies[i].Url = obj.baseUrl
			}
			if cookies[i].Url != "" {
				us, err := uurl.Parse(cookies[i].Url)
				if err != nil {
					return err
				}
				cookies[i].Domain = us.Hostname()
			}
		}
	}
	_, err = obj.webSock.NetworkSetCookies(ctx, cookies)
	return err
}
func (obj *Page) GetCookies(ctx context.Context, urls ...string) ([]cdp.Cookie, error) {
	if len(urls) == 0 {
		urls = append(urls, obj.baseUrl)
	}
	rs, err := obj.webSock.NetworkGetCookies(ctx, urls...)
	result := []cdp.Cookie{}
	if err != nil {
		return result, err
	}
	jsonData := tools.Any2json(rs.Result)
	for _, cookie := range jsonData.Get("cookies").Array() {
		var cook cdp.Cookie
		if err = json.Unmarshal(tools.StringToBytes(cookie.Raw), &cook); err != nil {
			return result, err
		}
		result = append(result, cook)
	}
	return result, nil
}
