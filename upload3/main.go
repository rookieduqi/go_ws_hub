package main

import (
	"bytes"
	"fmt"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
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

type FileUploadOut struct {
	Result    string
	Size      int64
	CheckSize int
	TmpPath   string
}

// UploadChunkHandler 处理单个分片上传请求
func UploadChunkHandler(c echo.Context) error {
	var dto RemoteFileUploadDto
	// 绑定 multipart/form-data 到 dto，Echo 会解析 form 数据
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

	// 设定存储分片的临时目录，使用文件hash来标识
	chunksDir := path.Join("/tmp", dto.Hash)
	if _, err := os.Stat(chunksDir); os.IsNotExist(err) {
		if err := os.MkdirAll(chunksDir, os.ModePerm); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"message": "创建临时目录失败：" + err.Error(),
			})
		}
	}

	// 构造当前分片的临时文件名，格式为: /tmp/{hash}/{hash}-{index}
	tmpFile := path.Join(chunksDir, dto.Hash+"-"+strconv.FormatInt(dto.Index, 10))

	// 检查文件块是否已经完整上传
	if info, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		if info.Size() == dto.Size {
			// 分片已上传且大小匹配，直接返回成功信息
			return c.JSON(http.StatusOK, map[string]interface{}{
				"msg": "该分片已上传",
			})
		}
		// 如果文件存在但大小不匹配，则删除后重新上传
		if err := os.Remove(tmpFile); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]interface{}{
				"msg": "删除损坏的分片失败: " + err.Error(),
			})
		}
	}
	// 打开或创建临时文件，用于追加写入分片数据
	fs, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"msg": "打开临时文件失败: " + err.Error(),
		})
	}
	defer fs.Close()

	// 获取上传的分片数据，表单字段为 "chunk"
	//fileHeader, err := c.FormFile("chunk")
	//if err != nil {
	//	return c.JSON(http.StatusBadRequest, map[string]interface{}{
	//		"message": "获取上传分片失败: " + err.Error(),
	//	})
	//}

	src, err := dto.File.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "打开上传分片失败: " + err.Error(),
		})
	}
	defer src.Close()

	// 将上传的分片数据写入临时文件
	if _, err = io.Copy(fs, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "写入分片数据失败: " + err.Error(),
		})
	}

	// 检查当前临时文件大小
	fi, err := fs.Stat()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "获取临时文件状态失败: " + err.Error(),
		})
	}
	currentSize := fi.Size()

	// 如果累计写入的大小与整个文件总大小相同，认为所有分片已上传完毕
	//if currentSize != dto.SliceSize {
	//	return c.JSON(http.StatusOK, map[string]interface{}{
	//		"msg":       "文件上传失败",
	//		"finalPath": tmpFile,
	//	})
	//}
	//
	// 返回当前分片上传成功信息
	return c.JSON(http.StatusOK, FileUploadOut{
		Result:    "分片上传成功",
		Size:      dto.Size,
		CheckSize: int(currentSize),
		TmpPath:   chunksDir,
	})
}

// MergeChunksCmdDto 定义合并分片接口的参数
type MergeChunksCmdDto struct {
	Hash       string `form:"hash" json:"hash" query:"hash" validate:"required"`                   // 文件hash，用于确定临时目录
	Total      int64  `form:"total" json:"total" query:"total" validate:"required"`                // 整个文件总大小（可用于校验）
	Name       string `form:"name" json:"name" query:"name" validate:"required"`                   // 文件原始名称
	UploadPath string `form:"uploadPath" json:"uploadPath" query:"uploadPath" validate:"required"` // 最终存储目录
}

// MergeChunksCmdHandler 通过命令方式合并分片并清理临时目录
func MergeChunksCmdHandler(c echo.Context) error {
	var dto MergeChunksCmdDto
	if err := c.Bind(&dto); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "参数绑定错误: " + err.Error(),
		})
	}

	// 构造分片存储的临时目录，假设在 global.ConfigInstance.Serve.UploadTmpPath 下，以 hash 命名
	chunksDir := path.Join("/tmp", dto.Hash)
	info, err := os.Stat(chunksDir)
	if err != nil || !info.IsDir() {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "分片临时目录不存在",
		})
	}

	// 构造最终文件完整路径：最终文件将放在 UploadPath 目录下，文件名为 Name
	finalFile := path.Join(dto.UploadPath, dto.Name)
	// 确保最终目录存在
	if err := os.MkdirAll(dto.UploadPath, os.ModePerm); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "创建最终存储目录失败: " + err.Error(),
		})
	}

	// 组装 shell 命令:
	// 1. 删除可能已存在的最终文件，防止合并时追加内容
	// 2. 利用 ls -1v 对目录下的文件进行自然排序，再用 cat 命令将所有文件合并到最终文件中
	// 3. 合并完成后删除临时目录
	//
	// 示例命令如下：
	//   rm -f "finalFile"; cat $(ls -1v "chunksDir"/*) > "finalFile"; rm -rf "chunksDir"
	//allFiles := path.Join("/tmp", dto.Hash, "*")
	//mergedCommand := fmt.Sprintf(`rm -rf "%s"; for file in $(ls -1v %s); do cat "$file" >> "%s"; done`, saveFile, allFiles, saveFile)
	dir := fmt.Sprintf("/tmp/%s", dto.Hash) // dto.Hash 需确保包含完整的目录名，如 "Charles 4.6.6.dmg_1711381809000"
	mergedCommand := fmt.Sprintf(`rm -f "%s"; cat $(ls -1v %s/*) > "%s"`, finalFile, dir, finalFile)
	//mergedCommand := fmt.Sprintf(`rm -f "%s"; cat $(ls -1v %s/*) > "%s"`, finalFile, chunksDir, finalFile)
	fmt.Println(mergedCommand)
	//mergedCommand := fmt.Sprintf(`rm -f "%s"; cat $(ls -1v "%s"/*) > "%s"; rm -rf "%s"`, saveFile, chunksDir, saveFile, chunksDir)

	// 执行命令
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.Command("sh", "-c", mergedCommand)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": fmt.Sprintf("合并命令执行失败: %v, 错误输出: %s", err, stderrBuf.String()),
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":   "文件合并成功",
		"finalFile": finalFile,
		"stdout":    stdoutBuf.String(),
	})
}

func main() {
	e := echo.New()

	e.Use(middleware.CORS())

	fileGroup := e.Group("file")
	{
		fileGroup.POST("/upload", UploadChunkHandler)
		fileGroup.POST("/chunks", MergeChunksCmdHandler)
	}

	log.Println("Relay server running on :8089")
	if err := e.Start(":8089"); err != nil {
		log.Fatal("Server run error:", err)
	}
}
