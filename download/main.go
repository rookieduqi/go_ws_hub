package download

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"

	"github.com/labstack/echo/v4"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// DownloadSftpHandler 通过 SSH 登陆远程服务器建立 SFTP 客户端，将指定远程文件下载给客户端
func DownloadSftpHandler(c echo.Context) error {
	// 从查询参数中获取远程文件路径
	remoteFilePath := c.QueryParam("filepath")
	if remoteFilePath == "" {
		return c.String(http.StatusBadRequest, "缺少远程文件路径参数")
	}

	// 可选：如果传入的是 URL 格式，可解析提取文件路径
	if u, err := url.Parse(remoteFilePath); err == nil && u.Scheme != "" {
		remoteFilePath = u.Path
	}

	// 配置 SSH 连接参数
	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("vUbFTsMJUY3AhpyT"),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// 建立 SSH 连接
	sshClient, err := ssh.Dial("tcp", "39.98.79.46:22", sshConfig)
	if err != nil {
		log.Printf("建立 SSH 连接失败：%v", err)
		return c.String(http.StatusInternalServerError, "建立 SSH 连接失败")
	}
	defer sshClient.Close()

	// 创建 SFTP 客户端
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		log.Printf("创建 SFTP 客户端失败：%v", err)
		return c.String(http.StatusInternalServerError, "创建 SFTP 客户端失败")
	}
	defer sftpClient.Close()

	// 获取文件信息
	fileInfo, err := sftpClient.Stat(remoteFilePath)
	if err != nil {
		log.Printf("获取文件信息失败：%v", err)
		return c.String(http.StatusInternalServerError, "获取文件信息失败")
	}

	// 打开远程文件
	remoteFile, err := sftpClient.OpenFile(remoteFilePath, os.O_RDONLY)
	if err != nil {
		log.Printf("打开远程文件失败：%v", err)
		return c.String(http.StatusInternalServerError, "打开远程文件失败")
	}
	defer remoteFile.Close()

	// 获取文件名作为下载时的文件名
	filename := path.Base(remoteFilePath)
	if filename == "" {
		filename = "downloaded_file"
	}

	// 设置响应头：通知浏览器以附件形式下载
	c.Response().Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	// 设置通用的二进制数据流（或根据实际情况设置 Content-Type）
	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("Content-Transfer-Encoding", "binary")
	c.Response().Header().Set("Expires", "0")
	c.Response().Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
	c.Response().WriteHeader(http.StatusOK)

	// 将远程文件内容通过流式传输发送给客户端
	if _, err := io.Copy(c.Response(), remoteFile); err != nil {
		log.Printf("传输文件内容失败：%v", err)
		return c.String(http.StatusInternalServerError, "传输文件内容失败")
	}
	return nil
}

//func main() {
//	e := echo.New()
//	e.Use(middleware.Logger())
//	e.Use(middleware.Recover())
//
//	// 前端可以调用类似如下接口进行下载：
//	// http://localhost:8080/download?filepath=/remote/path/to/file.txt
//	fileGroup := e.Group("file")
//	{
//		fileGroup.GET("/download", DownloadSftpHandler)
//	}
//
//	log.Println("服务器启动于 :8080")
//	if err := e.Start(":8080"); err != nil {
//		log.Fatalf("服务器启动失败：%v", err)
//	}
//}
