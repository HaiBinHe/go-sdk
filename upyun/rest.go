package upyun

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPartSize      = 1024 * 1024
	MaxPartNum           = 10000
	minResumePutFileSize = 10 * 1024 * 1024
	MaxListTries         = 5
	MaxLimit             = 4096
	DefaultLimit         = 256
)

type restReqConfig struct {
	method    string
	uri       string
	query     string
	headers   map[string]string
	closeBody bool
	httpBody  io.Reader
	useMD5    bool
}

// GetObjectConfig provides a configuration to Get method.
type GetObjectConfig struct {
	Path string
	// Headers contains custom http header, like User-Agent.
	Headers   map[string]string
	LocalPath string
	Writer    io.Writer
}

// GetObjectConfig provides a configuration to List method.
type GetObjectsConfig struct {
	Path           string
	Headers        map[string]string
	ObjectsChan    chan *FileInfo
	QuitChan       chan bool
	MaxListObjects int
	MaxListTries   int
	// MaxListLevel: depth of recursion
	MaxListLevel int
	// DescOrder:  whether list objects by desc-order
	DescOrder bool

	rootDir string
	level   int
	objNum  int
	try     int
}

// ListObjectsConfig list objects Config
type ListObjectsConfig struct {
	Path         string            // 文件路径or文件夹路径
	Headers      map[string]string // 请求头
	Iter         string            // 下次遍历目录的开始位置，第一次不需要输入，之后每次返回结果时会返回当前结束的位置, 即下次开始的位置
	MaxListTries int               // 重试的次数最大值
	DescOrder    bool              // 正序or倒叙, 默认正序
	Limit        int               // 每次遍历的文件个数，默认256 最大值为4096
}

type GetRequestConfig struct {
	Path    string
	Headers map[string]string
}

// PutObjectConfig provides a configuration to Put method.
type PutObjectConfig struct {
	Path            string
	LocalPath       string
	Reader          io.Reader
	Headers         map[string]string
	UseMD5          bool
	UseResumeUpload bool
	// Append Api Deprecated
	// AppendContent     bool
	ResumePartSize    int64
	MaxResumePutTries int
}

type MoveObjectConfig struct {
	SrcPath  string
	DestPath string
	Headers  map[string]string
}

type CopyObjectConfig struct {
	SrcPath  string
	DestPath string
	Headers  map[string]string
}

// UploadFileConfig is multipart file upload config
type UploadPartConfig struct {
	Reader   io.Reader
	PartSize int64
	PartID   int
}
type CompleteMultipartUploadConfig struct {
	Md5 string
}
type InitMultipartUploadConfig struct {
	Path          string
	PartSize      int64
	ContentLength int64 // optional
	ContentType   string
	OrderUpload   bool
}
type InitMultipartUploadResult struct {
	UploadID string
	Path     string
	PartSize int64
}

type DeleteObjectConfig struct {
	Path   string
	Async  bool
	Folder bool // optional
}

type ModifyMetadataConfig struct {
	Path      string
	Operation string
	Headers   map[string]string
}

type ListMultipartConfig struct {
	Prefix string
	Limit  int64
}
type ListMultipartPartsConfig struct {
	BeginID int
}
type MultipartUploadFile struct {
	Key       string `json:"key"`
	UUID      string `json:"uuid"`
	Completed bool   `json:"completed"`
	CreatedAt int64  `json:"created_at"`
}
type ListMultipartUploadResult struct {
	Files []*MultipartUploadFile `json:"files"`
}
type MultipartUploadedPart struct {
	Etag string `json:"etag"`
	Size int64  `json:"size"`
	Id   int    `json:"id"`
}
type ListUploadedPartsResult struct {
	Parts []*MultipartUploadedPart `json:"parts"`
}

func (up *UpYun) Usage() (n int64, err error) {
	var resp *http.Response
	resp, err = up.doRESTRequest(&restReqConfig{
		method: "GET",
		uri:    "/",
		query:  "usage",
	})

	if err == nil {
		n, err = readHTTPBodyToInt(resp)
	}

	if err != nil {
		return 0, errorOperation("usage", err)
	}
	return n, nil
}

func (up *UpYun) Mkdir(path string) error {
	_, err := up.doRESTRequest(&restReqConfig{
		method: "POST",
		uri:    path,
		headers: map[string]string{
			"folder":         "true",
			"x-upyun-folder": "true",
		},
		closeBody: true,
	})
	if err != nil {
		return errorOperation(fmt.Sprintf("mkdir %s", path), err)
	}
	return nil
}

func (up *UpYun) Get(config *GetObjectConfig) (fInfo *FileInfo, err error) {
	if config.LocalPath != "" {
		var fd *os.File
		if fd, err = os.Create(config.LocalPath); err != nil {
			return nil, errorOperation("create file", err)
		}
		defer fd.Close()
		config.Writer = fd
	}

	if config.Headers == nil {
		config.Headers = make(map[string]string)
	}

	config.Headers["x-upyun-folder"] = "false"

	if config.Writer == nil {
		return nil, errors.New("no writer")
	}

	resp, err := up.doRESTRequest(&restReqConfig{
		method:  "GET",
		uri:     config.Path,
		headers: config.Headers,
	})
	if err != nil {
		return nil, errorOperation(fmt.Sprintf("get %s", config.Path), err)
	}
	defer resp.Body.Close()

	fInfo = parseHeaderToFileInfo(resp.Header, false)
	fInfo.Name = config.Path

	if fInfo.Size, err = io.Copy(config.Writer, resp.Body); err != nil {
		return nil, errorOperation("io copy", err)
	}
	return
}

func (up *UpYun) put(config *PutObjectConfig) error {
	/* Append Api Deprecated
	if config.AppendContent {
		if config.Headers == nil {
			config.Headers = make(map[string]string)
		}
		config.Headers["X-Upyun-Append"] = "true"
	}
	*/
	_, err := up.doRESTRequest(&restReqConfig{
		method:    "PUT",
		uri:       config.Path,
		headers:   config.Headers,
		closeBody: true,
		httpBody:  config.Reader,
		useMD5:    config.UseMD5,
	})
	if err != nil {
		return errorOperation(fmt.Sprintf("put %s", config.Path), err)
	}
	return nil
}

func getPartInfo(partSize, fsize int64) (int64, int64, error) {
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	if partSize < DefaultPartSize {
		return 0, 0, fmt.Errorf("The minimum of part size is %d", DefaultPartSize)
	}
	if partSize%DefaultPartSize != 0 {
		return 0, 0, fmt.Errorf("The part size is a multiple of %d", DefaultPartSize)
	}

	partNum := (fsize + partSize - 1) / partSize
	if partNum > MaxPartNum {
		return 0, 0, fmt.Errorf("The maximum part number is  %d", MaxPartNum)
	}
	return partSize, partNum, nil
}

func (up *UpYun) Put(config *PutObjectConfig) (err error) {
	if config.LocalPath != "" {
		var fd *os.File
		if fd, err = os.Open(config.LocalPath); err != nil {
			return errorOperation("open file", err)
		}
		defer fd.Close()
		config.Reader = fd
	}

	if config.UseResumeUpload {
		return up.resumePut(config, nil)
	}
	return up.put(config)
}

func (up *UpYun) Move(config *MoveObjectConfig) error {
	headers := map[string]string{
		"X-Upyun-Move-Source": path.Join("/", up.Bucket, escapeUri(config.SrcPath)),
	}
	for k, v := range config.Headers {
		headers[k] = v
	}
	_, err := up.doRESTRequest(&restReqConfig{
		method:  "PUT",
		uri:     config.DestPath,
		headers: headers,
	})
	if err != nil {
		return errorOperation("move source", err)
	}
	return nil
}

func (up *UpYun) Copy(config *CopyObjectConfig) error {
	headers := map[string]string{
		"X-Upyun-Copy-Source": path.Join("/", up.Bucket, escapeUri(config.SrcPath)),
	}
	for k, v := range config.Headers {
		headers[k] = v
	}
	_, err := up.doRESTRequest(&restReqConfig{
		method:  "PUT",
		uri:     config.DestPath,
		headers: headers,
	})
	if err != nil {
		return errorOperation("copy source", err)
	}
	return nil
}

func (up *UpYun) InitMultipartUpload(config *InitMultipartUploadConfig) (*InitMultipartUploadResult, error) {
	partSize, _, err := getPartInfo(config.PartSize, config.ContentLength)
	if err != nil {
		return nil, errorOperation("init multipart", err)
	}
	headers := make(map[string]string)
	headers["X-Upyun-Multi-Type"] = config.ContentType
	if config.ContentLength > 0 {
		headers["X-Upyun-Multi-Length"] = strconv.FormatInt(config.ContentLength, 10)
	}
	headers["X-Upyun-Multi-Stage"] = "initiate"
	if !config.OrderUpload {
		headers["X-Upyun-Multi-Disorder"] = "true"
	}
	headers["X-Upyun-Multi-Part-Size"] = strconv.FormatInt(partSize, 10)
	resp, err := up.doRESTRequest(&restReqConfig{
		method:    "PUT",
		uri:       config.Path,
		headers:   headers,
		closeBody: true,
	})
	if err != nil {
		return nil, errorOperation("init multipart", err)
	}
	return &InitMultipartUploadResult{
		UploadID: resp.Header.Get("X-Upyun-Multi-Uuid"),
		Path:     config.Path,
		PartSize: partSize,
	}, nil
}
func (up *UpYun) UploadPart(initResult *InitMultipartUploadResult, part *UploadPartConfig) error {
	headers := make(map[string]string)
	headers["X-Upyun-Multi-Stage"] = "upload"
	headers["X-Upyun-Multi-Uuid"] = initResult.UploadID
	headers["X-Upyun-Part-Id"] = strconv.FormatInt(int64(part.PartID), 10)
	headers["Content-Length"] = strconv.FormatInt(part.PartSize, 10)

	_, err := up.doRESTRequest(&restReqConfig{
		method:    "PUT",
		uri:       initResult.Path,
		headers:   headers,
		closeBody: true,
		useMD5:    false,
		httpBody:  part.Reader,
	})
	if err != nil {
		return errorOperation("upload multipart", err)
	}
	return nil
}
func (up *UpYun) CompleteMultipartUpload(initResult *InitMultipartUploadResult, config *CompleteMultipartUploadConfig) error {
	headers := make(map[string]string)
	headers["X-Upyun-Multi-Stage"] = "complete"
	headers["X-Upyun-Multi-Uuid"] = initResult.UploadID
	if config != nil {
		if config.Md5 != "" {
			headers["X-Upyun-Multi-Md5"] = config.Md5
		}
	}
	_, err := up.doRESTRequest(&restReqConfig{
		method:  "PUT",
		uri:     initResult.Path,
		headers: headers,
	})
	if err != nil {
		return errorOperation("complete multipart", err)
	}
	return nil
}
func (up *UpYun) ListMultipartUploads(config *ListMultipartConfig) (*ListMultipartUploadResult, error) {
	headers := make(map[string]string)
	headers["X-Upyun-List-Type"] = "multi"
	if config.Prefix != "" {
		headers["X-Upyun-List-Prefix"] = base64.StdEncoding.EncodeToString([]byte(config.Prefix))
	}
	if config.Limit > 0 {
		headers["X-Upyun-List-Limit"] = strconv.FormatInt(config.Limit, 10)
	}

	res, err := up.doRESTRequest(&restReqConfig{
		method:    "GET",
		headers:   headers,
		uri:       "/",
		closeBody: false,
		useMD5:    false,
	})
	if err != nil {
		return nil, errorOperation("list multipart", err)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errorOperation("list multipart read body", err)
	}

	result := &ListMultipartUploadResult{}
	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, errorOperation("list multipart read body", err)
	}
	return result, nil
}

func (up *UpYun) ListMultipartParts(intiResult *InitMultipartUploadResult, config *ListMultipartPartsConfig) (*ListUploadedPartsResult, error) {
	headers := make(map[string]string)
	headers["X-Upyun-Multi-Uuid"] = intiResult.UploadID

	if config.BeginID > 0 {
		headers["X-Upyun-Part-Id"] = fmt.Sprint(config.BeginID)
	}
	res, err := up.doRESTRequest(&restReqConfig{
		method:    "GET",
		headers:   headers,
		uri:       intiResult.Path,
		closeBody: false,
		useMD5:    false,
	})
	if err != nil {
		return nil, errorOperation("list multipart parts", err)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errorOperation("list multipart parts read body", err)
	}

	result := &ListUploadedPartsResult{}
	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, errorOperation("list multipart parts read body", err)
	}
	return result, nil
}
func (up *UpYun) Delete(config *DeleteObjectConfig) error {
	headers := map[string]string{}
	if config.Async {
		headers["x-upyun-async"] = "true"
	}
	if config.Folder {
		headers["x-upyun-folder"] = "true"
	}
	_, err := up.doRESTRequest(&restReqConfig{
		method:    "DELETE",
		uri:       config.Path,
		headers:   headers,
		closeBody: true,
	})
	if err != nil {
		return errorOperation("delete", err)
	}
	return nil
}

// GetRequest return response
func (up *UpYun) GetRequest(config *GetRequestConfig) (*http.Response, error) {
	if config.Path == "" {
		return nil, errors.New("needed set config.Path")
	}

	resp, err := up.doRESTRequest(&restReqConfig{
		method:  "GET",
		uri:     config.Path,
		headers: config.Headers,
	})
	if err != nil {
		return nil, errorOperation(fmt.Sprintf("get %s", config.Path), err)
	}

	return resp, nil
}

func (up *UpYun) GetInfo(path string) (*FileInfo, error) {
	resp, err := up.doRESTRequest(&restReqConfig{
		method:    "HEAD",
		uri:       path,
		closeBody: true,
	})
	if err != nil {
		return nil, errorOperation("get info", err)
	}
	fInfo := parseHeaderToFileInfo(resp.Header, true)
	fInfo.Name = path
	return fInfo, nil
}

func (up *UpYun) List(config *GetObjectsConfig) error {
	if config.ObjectsChan == nil {
		return errors.New("ObjectsChan is nil")
	}
	if config.Headers == nil {
		config.Headers = make(map[string]string)
	}
	if config.QuitChan == nil {
		config.QuitChan = make(chan bool)
	}
	// 50 is nice value
	if _, exist := config.Headers["X-List-Limit"]; !exist {
		config.Headers["X-List-Limit"] = "50"
	}

	if config.DescOrder {
		config.Headers["X-List-Order"] = "desc"
	}

	config.Headers["X-UpYun-Folder"] = "true"
	config.Headers["Accept"] = "application/json"

	// 1st level
	if config.level == 0 {
		defer close(config.ObjectsChan)
	}

	for {
		resp, err := up.doRESTRequest(&restReqConfig{
			method:  "GET",
			uri:     config.Path,
			headers: config.Headers,
		})

		if err != nil {
			var nerr net.Error
			if ok := errors.As(err, &nerr); ok {
				config.try++
				if config.MaxListTries == 0 || config.try < config.MaxListTries {
					time.Sleep(10 * time.Millisecond)
					continue
				}
			}
			return errorOperation("list", err)
		}

		b, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return errorOperation("list read body", err)
		}

		iter, files, err := parseBodyToFileInfos(b)
		if err != nil {
			return errorOperation("list read body", err)
		}
		for _, fInfo := range files {
			if fInfo.IsDir && (config.level+1 < config.MaxListLevel || config.MaxListLevel == -1) {
				rConfig := &GetObjectsConfig{
					Path:           path.Join(config.Path, fInfo.Name),
					QuitChan:       config.QuitChan,
					ObjectsChan:    config.ObjectsChan,
					MaxListTries:   config.MaxListTries,
					MaxListObjects: config.MaxListObjects,
					DescOrder:      config.DescOrder,
					MaxListLevel:   config.MaxListLevel,
					level:          config.level + 1,
					rootDir:        path.Join(config.rootDir, fInfo.Name),
					try:            config.try,
					objNum:         config.objNum,
				}
				if err = up.List(rConfig); err != nil {
					return err
				}
				// empty folder
				if config.objNum == rConfig.objNum {
					fInfo.IsEmptyDir = true
				}
				config.try, config.objNum = rConfig.try, rConfig.objNum
			}
			if config.rootDir != "" {
				fInfo.Name = path.Join(config.rootDir, fInfo.Name)
			}

			select {
			case <-config.QuitChan:
				return nil
			default:
				config.ObjectsChan <- fInfo
			}

			config.objNum++
			if config.MaxListObjects > 0 && config.objNum >= config.MaxListObjects {
				return nil
			}
		}

		if iter == "g2gCZAAEbmV4dGQAA2VvZg" {
			return nil
		}
		config.Headers["X-List-Iter"] = iter
	}
}

func (up *UpYun) ListObjects(config *ListObjectsConfig) (fileInfos []*FileInfo, iter string, err error) {
	if config.Headers == nil {
		config.Headers = make(map[string]string)
	}

	if config.Limit == 0 || config.Limit > MaxLimit {
		config.Headers["X-List-Limit"] = strconv.Itoa(DefaultLimit)
	} else {
		config.Headers["X-List-Limit"] = strconv.Itoa(config.Limit)
	}

	if config.DescOrder {
		config.Headers["X-List-Order"] = "desc"
	}

	if config.Iter != "" {
		config.Headers["x-list-iter"] = config.Iter
	}

	if config.MaxListTries <= 0 {
		config.MaxListTries = MaxListTries
	}

	config.Headers["X-UpYun-Folder"] = "true"
	config.Headers["Accept"] = "application/json"
	var resp *http.Response
	try := 0
	for {
		resp, err = up.doRESTRequest(&restReqConfig{
			method:  "GET",
			uri:     config.Path,
			headers: config.Headers,
		})

		// 重试
		if err != nil {
			var nerr net.Error
			if ok := errors.As(err, &nerr); ok {
				try++
				if try < config.MaxListTries {
					time.Sleep(10 * time.Millisecond)
					continue
				}
			}
			return nil, "", errorOperation("list", err)
		}
		break
	}

	// 读取列表
	b, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, "", errorOperation("list read body", err)
	}

	iter, files, err := parseBodyToFileInfos(b)
	if err != nil {
		return nil, "", errorOperation("list read body", err)
	}

	if iter == "g2gCZAAEbmV4dGQAA2VvZg" {
		return files, "", nil
	}

	return files, iter, nil
}

func (up *UpYun) ModifyMetadata(config *ModifyMetadataConfig) error {
	if config.Operation == "" {
		config.Operation = "merge"
	}
	_, err := up.doRESTRequest(&restReqConfig{
		method:    "PATCH",
		uri:       config.Path,
		query:     "metadata=" + config.Operation,
		headers:   config.Headers,
		closeBody: true,
	})
	if err != nil {
		return errorOperation("modify metadata", err)
	}
	return nil
}

func (up *UpYun) doRESTRequest(config *restReqConfig) (*http.Response, error) {
	escUri := path.Join("/", up.Bucket, escapeUri(config.uri))
	if strings.HasSuffix(config.uri, "/") {
		escUri += "/"
	}
	if config.query != "" {
		escUri += "?" + config.query
	}

	headers := map[string]string{}
	hasMD5 := false
	for k, v := range config.headers {
		if strings.ToLower(k) == "content-md5" && v != "" {
			hasMD5 = true
		}
		headers[k] = v
	}

	headers["Date"] = makeRFC1123Date(time.Now())
	headers["Host"] = "v0.api.upyun.com"

	if !hasMD5 && config.useMD5 {
		switch v := config.httpBody.(type) {
		case *os.File:
			headers["Content-MD5"], _ = md5File(v)
		case UpYunPutReader:
			headers["Content-MD5"] = v.MD5()
		}
	}

	if up.deprecated {
		if _, ok := headers["Content-Length"]; !ok {
			size := int64(0)
			switch v := config.httpBody.(type) {
			case *os.File:
				if fInfo, err := v.Stat(); err == nil {
					size = fInfo.Size()
				}
			case UpYunPutReader:
				size = int64(v.Len())
			}
			headers["Content-Length"] = fmt.Sprint(size)
		}
		headers["Authorization"] = up.MakeRESTAuth(&RESTAuthConfig{
			Method:    config.method,
			Uri:       escUri,
			DateStr:   headers["Date"],
			LengthStr: headers["Content-Length"],
		})
	} else {
		headers["Authorization"] = up.MakeUnifiedAuth(&UnifiedAuthConfig{
			Method:     config.method,
			Uri:        escUri,
			DateStr:    headers["Date"],
			ContentMD5: headers["Content-MD5"],
		})
	}

	endpoint := up.doGetEndpoint("v0.api.upyun.com")
	url := fmt.Sprintf("http://%s%s", endpoint, escUri)

	resp, err := up.doHTTPRequest(config.method, url, headers, config.httpBody)
	if err != nil {
		return nil, err
	}

	if config.closeBody {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}

	return resp, nil
}

type BreakPointConfig struct {
	UploadID   string
	PartID     int
	PartSize   int64
	MaxPartID  int
	UseMD5     bool
	ContentMd5 string
}

func (up *UpYun) ResumePut(config *PutObjectConfig) (err error) {
	if config.LocalPath != "" {
		var fd *os.File
		if fd, err = os.Open(config.LocalPath); err != nil {
			return errorOperation("open file", err)
		}
		defer fd.Close()
		config.Reader = fd
	}
	breakPoint, err := up.Recoder.Get(up.Recoder.UploadID)
	if err != nil {
		return err
	}
	return up.resumePut(config, breakPoint)
}

func (up *UpYun) resumePut(config *PutObjectConfig, breakpoint *BreakPointConfig) error {
	f, ok := config.Reader.(*os.File)
	if !ok {
		return errors.New("resumePut: type != *os.File")
	}

	fileinfo, err := f.Stat()
	if err != nil {
		return errorOperation("stat", err)
	}

	fsize := fileinfo.Size()
	if fsize < minResumePutFileSize {
		return up.put(config)
	}

	if config.ResumePartSize == 0 {
		config.ResumePartSize = DefaultPartSize
	}

	if config.Headers == nil {
		config.Headers = make(map[string]string)
	}
	headers := config.Headers

	// first upload
	var uploadInfo *InitMultipartUploadResult
	if breakpoint == nil {
		uploadInfo, err = up.InitMultipartUpload(&InitMultipartUploadConfig{
			Path:          config.Path,
			PartSize:      config.ResumePartSize,
			ContentType:   headers["Content-Type"],
			ContentLength: fsize,
			OrderUpload:   true,
		})
		if err != nil {
			return err
		}

		maxPartID := int((fsize+uploadInfo.PartSize-1)/uploadInfo.PartSize - 1)
		breakpoint = &BreakPointConfig{
			UploadID:  uploadInfo.UploadID,
			PartSize:  uploadInfo.PartSize,
			PartID:    0,
			MaxPartID: maxPartID,
		}
	}

	err = up.resumeUploadPart(config, breakpoint, f, fileinfo)
	if err != nil {
		return err
	}

	completeConfig := &CompleteMultipartUploadConfig{}
	if config.UseMD5 {
		f.Seek(0, 0)
		completeConfig.Md5, _ = md5File(f)
	}

	return up.CompleteMultipartUpload(
		&InitMultipartUploadResult{
			UploadID: breakpoint.UploadID,
			Path:     config.Path,
			PartSize: breakpoint.PartSize,
		}, completeConfig)
}

func (up *UpYun) resumeUploadPart(config *PutObjectConfig, breakpoint *BreakPointConfig, f *os.File, fileInfo fs.FileInfo) error {
	up.Recoder.UploadID = breakpoint.UploadID
	fsize := int64(breakpoint.MaxPartID+1) * breakpoint.PartSize
	maxPartID := breakpoint.MaxPartID
	partID := breakpoint.PartID
	curSize, partSize := int64(partID)*breakpoint.PartSize, breakpoint.PartSize

	if breakpoint.UseMD5 {
		if breakpoint.ContentMd5 != "" {
			// 判断之前上传的文件是否发生了修改
			partFile, err := newFragmentFile(f, int64(partID+1)*partSize, partSize)
			if err != nil {
				return errorOperation("new fragment file", err)
			}
			fmd5, err := fileBufMd5(partFile)
			if err != nil {
				return err
			}
			if fmd5 != breakpoint.ContentMd5 {
				return err
			}
		}
	}

	if isFileExpired(fileInfo, fsize) {
		return errors.New("resume file has expired")
	}

	for id := partID; id <= maxPartID; id++ {
		if curSize+partSize > fsize {
			partSize = fsize - curSize
		}

		fragFile, err := newFragmentFile(f, curSize, partSize)
		if err != nil {
			return errorOperation("new fragment file", err)
		}

		try := 0
		for ; config.MaxResumePutTries == 0 || try < config.MaxResumePutTries; try++ {
			err = up.UploadPart(
				&InitMultipartUploadResult{
					UploadID: breakpoint.UploadID,
					Path:     config.Path,
					PartSize: breakpoint.PartSize,
				},
				&UploadPartConfig{
					PartID:   id,
					PartSize: partSize,
					Reader:   fragFile,
				})
			if err == nil {
				break
			}
		}

		if config.MaxResumePutTries > 0 && try == config.MaxResumePutTries {
			partFile, err := newFragmentFile(f, int64(partID+1)*partSize, partSize)
			if err != nil {
				return errorOperation("new fragment file", err)
			}
			fmd5, err := fileBufMd5(partFile)
			if err != nil {
				return err
			}

			breakpoint.PartID = id
			breakpoint.ContentMd5 = fmd5

			return up.Recoder.Set(breakpoint)
		}
		curSize += partSize
	}

	return nil
}
