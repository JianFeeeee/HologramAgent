package client

// BinaryChunker 提供二进制数据分块传输功能。
// 用于将大体积数据（如录像 mp4、大图片）按分块协议发送。
// 协议:
//   cmd_data_start {op, req_id, kind, total, chunk_size, mime}  —— 文本帧
//   <N 个二进制帧 0x2>                                          —— data bytes
//   cmd_data_end   {op, req_id, status:ok|error, error?}        —— 文本帧

const (
	// DefaultChunkSize 默认分块大小（8KB）
	DefaultChunkSize = 8192

	// MaxBinaryFrameSize 二进制帧最大大小（8MB）
	MaxBinaryFrameSize = 8 << 20
)

// ChunkCallback 分块发送回调，用于逐块处理。
type ChunkCallback func(chunk []byte) error

// ===== 数据聚合（接收端） =====

// DataAccumulator 聚合从设备接收的二进制分块数据。
type DataAccumulator struct {
	ReqID  string
	Kind   string
	MIME   string
	Total  int
	Got    int
	Chunks [][]byte
}

// Append 追加一块数据。
func (da *DataAccumulator) Append(chunk []byte) {
	da.Chunks = append(da.Chunks, chunk)
	da.Got += len(chunk)
}

// Assemble 聚合所有分块为完整数据。
func (da *DataAccumulator) Assemble() []byte {
	total := 0
	for _, c := range da.Chunks {
		total += len(c)
	}
	data := make([]byte, 0, total)
	for _, c := range da.Chunks {
		data = append(data, c...)
	}
	return data
}

// ExceededLimit 检查是否超出限制（声明的 2 倍或硬上限 64MB）。
func (da *DataAccumulator) ExceededLimit() bool {
	limit := da.Total*2 + 1024
	if limit < 64<<20 {
		limit = 64 << 20
	}
	return da.Got > limit
}
