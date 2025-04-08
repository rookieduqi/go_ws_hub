package main

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

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

// UploadChunkHandler 处理单个分片上传请求
func UploadChunkHandler(c echo.Context) error {
	var dto RemoteFileUploadDto

	// 绑定 multipart/form-data 到 dto；Echo 会解析 form 并自动给各字段赋值
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

	// 获取必要参数：
	// fileHash：文件的唯一标识（例如客户端计算的 MD5/SHA256）
	// index：当前分片索引，从 0 开始
	// total：总分片数
	// 注意：其他参数（如 fileName、sliceSize、totalSize）可根据业务需要传递
	fileHash := c.FormValue("fileHash")
	indexStr := c.FormValue("index")
	totalStr := c.FormValue("total")
	if fileHash == "" || indexStr == "" || totalStr == "" {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "缺少必要参数",
		})
	}

	// 解析分片索引和总分片数
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "index 参数错误",
		})
	}
	total, err := strconv.Atoi(totalStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "total 参数错误",
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

	// 构造临时存储目录，例如 "upload_tmp/<fileHash>/"
	tmpDir := path.Join("upload_tmp", fileHash)
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "创建临时目录失败：" + err.Error(),
		})
	}
	// 临时分片文件名称，如 "chunk_0", "chunk_1", ...
	chunkFilename := path.Join(tmpDir, fmt.Sprintf("chunk_%d", index))
	dst, err := os.Create(chunkFilename)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "创建临时分片文件失败：" + err.Error(),
		})
	}
	defer dst.Close()

	// 写入分片数据
	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "写入分片数据失败：" + err.Error(),
		})
	}

	// 可选：记录上传进度信息（例如存储到 Redis 或数据库），便于前端查询上传状态

	// 如果当前分片是最后一块，则触发合并操作
	if index == total-1 {
		// 合并所有分片到目标文件
		finalDir := "upload_final"
		if err := os.MkdirAll(finalDir, os.ModePerm); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"message": "创建最终文件目录失败：" + err.Error(),
			})
		}
		// 可选：原始文件名可以从其他参数中获取
		finalFilename := path.Join(finalDir, fileHash+"_merged")
		finalFile, err := os.Create(finalFilename)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"message": "创建最终文件失败：" + err.Error(),
			})
		}
		defer finalFile.Close()

		// 按顺序合并各个分片
		for i := 0; i < total; i++ {
			chunkPath := path.Join(tmpDir, fmt.Sprintf("chunk_%d", i))
			chunkFile, err := os.Open(chunkPath)
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
		// 合并完成后可删除临时目录，或保留以备重传验证
		// os.RemoveAll(tmpDir)

		// 返回合并结果（例如文件路径或成功消息）
		return c.JSON(http.StatusOK, map[string]interface{}{
			"message": "上传完成，文件已合并",
			"file":    finalFilename,
		})
	}

	// 分片上传成功，等待其它分片上传完成
	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":    "分片上传成功",
		"chunkIndex": index,
		"total":      total,
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
	}

	e.Logger.Fatal(e.Start(":8080"))
}
