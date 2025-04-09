package main

import (
	"errors"
	"fmt"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// sftp
type SftpPathLib struct {
	path   string
	client *sftp.Client
}

func NewSftpPathLib(path string, client *sftp.Client) *SftpPathLib {
	return &SftpPathLib{path: path, client: client}
}

func (f *SftpPathLib) Exists() (bool, error) {
	_, err := f.client.Lstat(f.path)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

func (f *SftpPathLib) Remove() error {
	// 检查文件是否存在
	ok, err := f.Exists()
	if err != nil {
		return err
	}

	// 文件不存在，直接返回
	if !ok {
		return nil
	}

	// 删除文件或目录
	return f.client.Remove(f.path)
}

func (f *SftpPathLib) TempPath(tmp, hash string) string {
	return path.Join(tmp, hash, "/")
}

func (f *SftpPathLib) MkdirAll() error {
	return f.client.MkdirAll(f.path)
}

func (f *SftpPathLib) Size() (int64, error) {
	info, err := f.client.Stat(f.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

const BuffSize = 1024 * 256

// 定义 DTO，用于绑定表单字段
type RemoteFileUploadDto struct {
	File       *multipart.FileHeader `form:"file" json:"file"`
	Index      int64                 `form:"index" json:"index"`
	Hash       string                `form:"hash"  json:"hash"`
	Size       int64                 `form:"size"  json:"size"`
	SliceSize  int64                 `form:"sliceSize" json:"sliceSize"`
	Total      int64                 `form:"total" json:"total"`
	Name       string                `form:"name"  json:"name"`
	UploadPath string                `form:"uploadPath" json:"uploadPath"`
	Now        int64                 `form:"now"   json:"now"`
	Extra      string                `form:"extra" json:"extra"`
}

type SftpFileUploadOut struct {
	Result    string
	Size      int64
	CheckSize int
	TmpPath   string
}

// 初始化客户端
func initSftpClient(conn *ssh.Client) (*sftp.Client, error) {
	size := 32768
	c, err := sftp.NewClient(conn, sftp.MaxPacket(size))
	if err != nil {
		return nil, errors.New("sftp connection error: " + err.Error())
	}
	return c, nil
}

// UploadChunkHandler 处理单个分片上传请求
func UploadChunkHandler(c echo.Context) error {
	var dto RemoteFileUploadDto

	// 绑定 multipart/form-data 到 dto;Echo 会解析 form 并自动给各字段赋值
	if err := c.Bind(&dto); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "参数绑定错误: " + err.Error(),
		})
	}

	if dto.File == nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "缺少文件字段 file",
		})
	}

	// 配置 SSH 客户端参数
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// 建立 SSH 连接
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{"msg": "SSH Dial error: " + err.Error()})
	}
	defer sshClient.Close()
	sftpClient, err := initSftpClient(sshClient)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"msg": "sftp connection error: " + err.Error(),
		})
	}
	defer sftpClient.Close()

	// 检查上传目录是否存在以及文件是否已存在
	chunksPath := path.Join("/tmp", dto.Hash, "/")
	chunksPathLib := NewSftpPathLib(chunksPath, sftpClient)
	isExists, err := chunksPathLib.Exists()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"msg": "sftp connection error: " + err.Error(),
		})
	}

	if !isExists {
		err = chunksPathLib.MkdirAll()
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]interface{}{
				"msg": "sftp connection error: " + err.Error(),
			})
		}
	}

	// 检查是否存在已无损上传的分片，如果存在，则直接返回上传成功
	// 检查文件块是否已经完整上传
	tmpFile := path.Join(chunksPath + "/" + dto.Hash + "-" + strconv.FormatInt(dto.Index, 10))
	tmpPathLib := NewSftpPathLib(tmpFile, sftpClient)
	isTmpPathExists, err := tmpPathLib.Exists()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"msg": "sftp connection error: " + err.Error(),
		})
	}

	if isTmpPathExists {
		// 获取已上传块的大小
		sourceSize, err := tmpPathLib.Size()
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]interface{}{
				"msg": "sftp connection error: " + err.Error(),
			})
		}
		// 如果已上传块的大小与期望的块大小匹配，返回上传成功
		if sourceSize == dto.SliceSize {
			return c.JSON(http.StatusOK, map[string]interface{}{
				"msg": "文件已上传",
			})
		}

		err = tmpPathLib.Remove() // 删除损坏的分片
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]interface{}{
				"msg": "删除损坏分片失败" + err.Error(),
			})
		}
	}

	// 打开（或创建）临时文件用于上传
	fs, err := sftpClient.OpenFile(tmpFile, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"msg": "sftp OpenFile error: " + err.Error(),
		})
	}

	// 获取上传的分片文件，字段名为 "chunk"
	fileHeader, err := c.FormFile("chunk")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "获取上传分片失败：" + err.Error(),
		})
	}

	src, err := fileHeader.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "打开上传分片失败：" + err.Error(),
		})
	}
	defer src.Close()

	// 写入分片数据
	if _, err = io.Copy(fs, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "写入分片数据失败：" + err.Error(),
		})
	}
	fileInfo, _ := fs.Stat()
	// 分片上传成功，等待其它分片上传完成
	return c.JSON(http.StatusOK, SftpFileUploadOut{
		Result:    "",
		Size:      dto.Size,
		CheckSize: int(fileInfo.Size()),
		TmpPath:   chunksPath,
	})
}

// MergeChunksHandler 通过 SSH/SFTP 将临时的分片文件合并为最终文件
func MergeChunksHandler(c echo.Context) error {
	// 从请求中获取文件标识和总分片数
	hash := c.FormValue("hash")
	totalStr := c.FormValue("total")
	if hash == "" || totalStr == "" {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "缺少必要参数: hash和total",
		})
	}
	total, err := strconv.Atoi(totalStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "参数 total 格式不正确",
		})
	}

	// SSH配置，可考虑从配置或环境变量读取敏感信息
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// 建立SSH连接
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "SSH Dial error: " + err.Error(),
		})
	}
	defer sshClient.Close()

	sftpClient, err := initSftpClient(sshClient)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "sftp connection error: " + err.Error(),
		})
	}
	defer sftpClient.Close()

	// 临时分片目录，例如 /tmp/<fileHash>/
	tmpDir := path.Join("/tmp", hash)
	// 最终合并文件目录，例如 /upload_final/
	finalDir := "/upload_final"
	if err := sftpClient.MkdirAll(finalDir); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "创建最终文件目录失败：" + err.Error(),
		})
	}
	finalFilename := path.Join(finalDir, hash+"_merged")

	// 在远程服务器上创建最终文件（覆盖或新建）
	finalFile, err := sftpClient.Create(finalFilename)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "创建最终文件失败：" + err.Error(),
		})
	}
	defer finalFile.Close()

	// 按顺序合并所有分片：分片文件命名为 "<hash>-<index>"
	for i := 0; i < total; i++ {
		chunkFilePath := path.Join(tmpDir, fmt.Sprintf("%s-%d", hash, i))
		chunkFile, err := sftpClient.Open(chunkFilePath)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"message": fmt.Sprintf("打开分片 %d 失败：%v", i, err),
			})
		}
		_, err = io.Copy(finalFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"message": fmt.Sprintf("合并分片 %d 失败：%v", i, err),
			})
		}
	}

	// 可选：合并完成后删除临时分片目录
	// sftpClient.RemoveDirectory(tmpDir)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"message": "上传完成，文件已合并",
		"file":    finalFilename,
	})
}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// 注册分片上传接口，例如 URL: POST /upload/chunk
	fileGroup := e.Group("files")
	{
		fileGroup.POST("remote_upload", UploadChunkHandler)
		fileGroup.POST("chunks", MergeChunksHandler)
	}

	e.Logger.Fatal(e.Start(":8080"))
}
