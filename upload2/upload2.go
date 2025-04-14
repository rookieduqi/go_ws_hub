package upload2

import (
	"github.com/labstack/echo/v4"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"sort"
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

// MergeChunksDto 用于绑定合并接口的参数
type MergeChunksDto struct {
	Hash       string `form:"hash" json:"hash" query:"hash" validate:"required"`                   // 用于唯一标识文件，存放在 /tmp/{hash} 目录中
	SliceSize  int64  `form:"sliceSize" json:"sliceSize" query:"sliceSize" validate:"required"`    // 每个分片的标准大小（字节）
	Total      int64  `form:"total" json:"total" query:"total" validate:"required"`                // 整个文件总大小（字节）
	Name       string `form:"name" json:"name" query:"name" validate:"required"`                   // 文件原始名称（最终文件名）
	UploadPath string `form:"uploadPath" json:"uploadPath" query:"uploadPath" validate:"required"` // 最终存储目录
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
	if currentSize != dto.SliceSize {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"msg":       "文件上传失败",
			"finalPath": tmpFile,
		})
	}

	// 返回当前分片上传成功信息
	return c.JSON(http.StatusOK, FileUploadOut{
		Result:    "分片上传成功",
		Size:      dto.Size,
		CheckSize: int(currentSize),
		TmpPath:   chunksDir,
	})
}

// mergeChunks 将 chunksDir 目录下所有分片合并成 finalFile
// 假设每个分片文件名格式为 "{hash}-{index}"
func mergeChunks(chunksDir, hash, finalFile string) error {
	// 创建或覆盖最终文件
	out, err := os.Create(finalFile)
	if err != nil {
		return err
	}
	defer out.Close()

	// 读取目录下所有文件
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		return err
	}

	// 只处理文件（不处理子目录），并将所有文件名存入切片
	var chunkFiles []string
	for _, entry := range entries {
		if !entry.IsDir() {
			chunkFiles = append(chunkFiles, entry.Name())
		}
	}

	// 按照分片索引排序。文件名格式为 "{hash}-{index}"，这里提取 '-' 后面的数字作为索引
	sort.Slice(chunkFiles, func(i, j int) bool {
		getIndex := func(filename string) int64 {
			// 找到最后一个 '-' 的位置
			pos := -1
			for i, ch := range filename {
				if ch == '-' {
					pos = i
				}
			}
			if pos >= 0 && pos+1 < len(filename) {
				idx, err := strconv.ParseInt(filename[pos+1:], 10, 64)
				if err == nil {
					return idx
				}
			}
			return 0
		}
		return getIndex(chunkFiles[i]) < getIndex(chunkFiles[j])
	})

	// 依次读取每个分片并写入最终文件
	for _, chunkName := range chunkFiles {
		chunkPath := path.Join(chunksDir, chunkName)
		in, err := os.Open(chunkPath)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// MergeChunksHandler 用于将分片合并成完整文件，清理临时目录
func MergeChunksHandler(c echo.Context) error {
	var dto MergeChunksDto
	if err := c.Bind(&dto); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "参数绑定错误: " + err.Error(),
		})
	}

	// 构造临时分片目录，假设为 /tmp/{hash}
	chunksDir := path.Join("/tmp", dto.Hash)
	info, err := os.Stat(chunksDir)
	if err != nil || !info.IsDir() {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "分片临时目录不存在",
		})
	}

	// 计算预期的分片数（考虑最后一个分片可能比标准分片小）
	expectedChunks := dto.Total / dto.SliceSize
	if dto.Total%dto.SliceSize != 0 {
		expectedChunks++
	}

	// 读取临时目录下分片数量
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "读取临时目录失败: " + err.Error(),
		})
	}
	if int64(len(entries)) < expectedChunks {
		return c.JSON(http.StatusBadRequest, map[string]interface{}{
			"message": "未完成所有分片上传，当前分片数量: " + strconv.Itoa(len(entries)) + "，预期: " + strconv.FormatInt(expectedChunks, 10),
		})
	}

	// 构造最终文件完整路径：UploadPath目录下的 Name 文件
	finalFile := path.Join(dto.UploadPath, dto.Name)
	// 进行合并操作
	if err := mergeChunks(chunksDir, dto.Hash, finalFile); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]interface{}{
			"message": "文件合并失败: " + err.Error(),
		})
	}

	// 删除临时分片目录，清理数据
	if err := os.RemoveAll(chunksDir); err != nil {
		// 如果删除失败可以记录日志，但返回成功信息
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":   "文件合并成功",
		"finalFile": finalFile,
	})
}

// getDirSize 遍历指定目录下所有文件，并返回文件总大小
func getDirSize(dir string) (int64, error) {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return 0, err
			}
			total += info.Size()
		}
	}
	return total, nil
}
