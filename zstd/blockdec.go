// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/huff0"
	"github.com/klauspost/compress/zstd/internal/xxhash"
)

type blockType uint8

//go:generate stringer -type=blockType,literalsBlockType,seqCompMode,tableIndex

const (
	blockTypeRaw blockType = iota
	blockTypeRLE
	blockTypeCompressed
	blockTypeReserved
)

type literalsBlockType uint8

const (
	literalsBlockRaw literalsBlockType = iota
	literalsBlockRLE
	literalsBlockCompressed
	literalsBlockTreeless
)

const (
	// maxCompressedBlockSize is the biggest allowed compressed block size (128KB)
	maxCompressedBlockSize = 128 << 10

	// Maximum possible block size (all Raw+Uncompressed).
	maxBlockSize = (1 << 21) - 1

	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#literals_section_header
	maxCompressedLiteralSize = 1 << 18
	maxRLELiteralSize        = 1 << 20
	maxMatchLen              = 131074
	maxSequences             = 0x7f00 + 0xffff

	// We support slightly less than the reference decoder to be able to
	// use ints on 32 bit archs.
	maxOffsetBits = 30
)

var (
	huffDecoderPool = sync.Pool{New: func() interface{} {
		return &huff0.Scratch{}
	}}

	fseDecoderPool = sync.Pool{New: func() interface{} {
		return &fseDecoder{}
	}}
)

type blockDec struct {
	// Raw source data of the block.
	data        []byte
	dataStorage []byte

	// Destination of the decoded data.
	dst []byte

	// Buffer for literals data.
	literalBuf []byte

	// Window size of the block.
	WindowSize uint64

	err error

	// Frame to use for singlethreaded decoding.
	// Should not be used by the decoder itself since parent may be another frame.
	localFrame *frameDec

	async struct {
		newHist  *history
		literals []byte
		seqData  []byte
		sequence []seqVals
	}

	// Block is RLE, this is the size.
	RLESize uint32
	tmp     [4]byte

	Type blockType

	// Is this the last block of a frame?
	Last bool

	// Use less memory
	lowMem bool
}

func (b *blockDec) String() string {
	if b == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Steam Size: %d, Type: %v, Last: %t, Window: %d", len(b.data), b.Type, b.Last, b.WindowSize)
}

func newBlockDec(lowMem bool) *blockDec {
	b := blockDec{
		lowMem: lowMem,
	}
	return &b
}

// reset will reset the block.
// Input must be a start of a block and will be at the end of the block when returned.
func (b *blockDec) reset(br byteBuffer, windowSize uint64) error {
	b.WindowSize = windowSize
	tmp, err := br.readSmall(3)
	if err != nil {
		println("Reading block header:", err)
		return err
	}
	bh := uint32(tmp[0]) | (uint32(tmp[1]) << 8) | (uint32(tmp[2]) << 16)
	b.Last = bh&1 != 0
	b.Type = blockType((bh >> 1) & 3)
	// find size.
	cSize := int(bh >> 3)
	maxSize := maxBlockSize
	switch b.Type {
	case blockTypeReserved:
		return ErrReservedBlockType
	case blockTypeRLE:
		b.RLESize = uint32(cSize)
		if b.lowMem {
			maxSize = cSize
		}
		cSize = 1
	case blockTypeCompressed:
		if debugDecoder {
			println("Data size on stream:", cSize)
		}
		b.RLESize = 0
		maxSize = maxCompressedBlockSize
		if windowSize < maxCompressedBlockSize && b.lowMem {
			maxSize = int(windowSize)
		}
		if cSize > maxCompressedBlockSize || uint64(cSize) > b.WindowSize {
			if debugDecoder {
				printf("compressed block too big: csize:%d block: %+v\n", uint64(cSize), b)
			}
			return ErrCompressedSizeTooBig
		}
	case blockTypeRaw:
		b.RLESize = 0
		// We do not need a destination for raw blocks.
		maxSize = -1
	default:
		panic("Invalid block type")
	}

	// Read block data.
	if cap(b.dataStorage) < cSize {
		if b.lowMem || cSize > maxCompressedBlockSize {
			b.dataStorage = make([]byte, 0, cSize)
		} else {
			b.dataStorage = make([]byte, 0, maxCompressedBlockSize)
		}
	}
	if cap(b.dst) <= maxSize {
		b.dst = make([]byte, 0, maxSize+1)
	}
	b.data, err = br.readBig(cSize, b.dataStorage)
	if err != nil {
		if debugDecoder {
			println("Reading block:", err, "(", cSize, ")", len(b.data))
			printf("%T", br)
		}
		return err
	}
	return nil
}

// sendEOF will make the decoder send EOF on this frame.
func (b *blockDec) sendErr(err error) {
	b.Last = true
	b.Type = blockTypeReserved
	b.err = err
}

// Close will release resources.
// Closed blockDec cannot be reset.
func (b *blockDec) Close() {
}

// decodeBuf
func (b *blockDec) decodeBuf(hist *history) error {
	switch b.Type {
	case blockTypeRLE:
		if cap(b.dst) < int(b.RLESize) {
			if b.lowMem {
				b.dst = make([]byte, b.RLESize)
			} else {
				b.dst = make([]byte, maxBlockSize)
			}
		}
		b.dst = b.dst[:b.RLESize]
		v := b.data[0]
		for i := range b.dst {
			b.dst[i] = v
		}
		hist.appendKeep(b.dst)
		return nil
	case blockTypeRaw:
		hist.appendKeep(b.data)
		return nil
	case blockTypeCompressed:
		saved := b.dst
		b.dst = hist.b
		hist.b = nil
		err := b.decodeCompressed(hist)
		if debugDecoder {
			println("Decompressed to total", len(b.dst), "bytes, hash:", xxhash.Sum64(b.dst), "error:", err)
		}
		hist.b = b.dst
		b.dst = saved
		return err
	case blockTypeReserved:
		// Used for returning errors.
		return b.err
	default:
		panic("Invalid block type")
	}
}

func (b *blockDec) decodeLiterals(in []byte, hist *history) (remain []byte, err error) {
	// There must be at least one byte for Literals_Block_Type and one for Sequences_Section_Header
	if len(in) < 2 {
		return in, ErrBlockTooSmall
	}

	litType := literalsBlockType(in[0] & 3)
	var litRegenSize int
	var litCompSize int
	sizeFormat := (in[0] >> 2) & 3
	var fourStreams bool
	var literals []byte
	switch litType {
	case literalsBlockRaw, literalsBlockRLE:
		switch sizeFormat {
		case 0, 2:
			// Regenerated_Size uses 5 bits (0-31). Literals_Section_Header uses 1 byte.
			litRegenSize = int(in[0] >> 3)
			in = in[1:]
		case 1:
			// Regenerated_Size uses 12 bits (0-4095). Literals_Section_Header uses 2 bytes.
			litRegenSize = int(in[0]>>4) + (int(in[1]) << 4)
			in = in[2:]
		case 3:
			//  Regenerated_Size uses 20 bits (0-1048575). Literals_Section_Header uses 3 bytes.
			if len(in) < 3 {
				println("too small: litType:", litType, " sizeFormat", sizeFormat, len(in))
				return in, ErrBlockTooSmall
			}
			litRegenSize = int(in[0]>>4) + (int(in[1]) << 4) + (int(in[2]) << 12)
			in = in[3:]
		}
	case literalsBlockCompressed, literalsBlockTreeless:
		switch sizeFormat {
		case 0, 1:
			// Both Regenerated_Size and Compressed_Size use 10 bits (0-1023).
			if len(in) < 3 {
				println("too small: litType:", litType, " sizeFormat", sizeFormat, len(in))
				return in, ErrBlockTooSmall
			}
			n := uint64(in[0]>>4) + (uint64(in[1]) << 4) + (uint64(in[2]) << 12)
			litRegenSize = int(n & 1023)
			litCompSize = int(n >> 10)
			fourStreams = sizeFormat == 1
			in = in[3:]
		case 2:
			fourStreams = true
			if len(in) < 4 {
				println("too small: litType:", litType, " sizeFormat", sizeFormat, len(in))
				return in, ErrBlockTooSmall
			}
			n := uint64(in[0]>>4) + (uint64(in[1]) << 4) + (uint64(in[2]) << 12) + (uint64(in[3]) << 20)
			litRegenSize = int(n & 16383)
			litCompSize = int(n >> 14)
			in = in[4:]
		case 3:
			fourStreams = true
			if len(in) < 5 {
				println("too small: litType:", litType, " sizeFormat", sizeFormat, len(in))
				return in, ErrBlockTooSmall
			}
			n := uint64(in[0]>>4) + (uint64(in[1]) << 4) + (uint64(in[2]) << 12) + (uint64(in[3]) << 20) + (uint64(in[4]) << 28)
			litRegenSize = int(n & 262143)
			litCompSize = int(n >> 18)
			in = in[5:]
		}
	}
	if debugDecoder {
		println("literals type:", litType, "litRegenSize:", litRegenSize, "litCompSize:", litCompSize, "sizeFormat:", sizeFormat, "4X:", fourStreams)
	}

	switch litType {
	case literalsBlockRaw:
		if len(in) < litRegenSize {
			println("too small: litType:", litType, " sizeFormat", sizeFormat, "remain:", len(in), "want:", litRegenSize)
			return in, ErrBlockTooSmall
		}
		literals = in[:litRegenSize]
		in = in[litRegenSize:]
		//printf("Found %d uncompressed literals\n", litRegenSize)
	case literalsBlockRLE:
		if len(in) < 1 {
			println("too small: litType:", litType, " sizeFormat", sizeFormat, "remain:", len(in), "want:", 1)
			return in, ErrBlockTooSmall
		}
		if cap(b.literalBuf) < litRegenSize {
			if b.lowMem {
				b.literalBuf = make([]byte, litRegenSize)
			} else {
				if litRegenSize > maxCompressedLiteralSize {
					// Exceptional
					b.literalBuf = make([]byte, litRegenSize)
				} else {
					b.literalBuf = make([]byte, litRegenSize, maxCompressedLiteralSize)
				}
			}
		}
		literals = b.literalBuf[:litRegenSize]
		v := in[0]
		for i := range literals {
			literals[i] = v
		}
		in = in[1:]
		if debugDecoder {
			printf("Found %d RLE compressed literals\n", litRegenSize)
		}
	case literalsBlockTreeless:
		if len(in) < litCompSize {
			println("too small: litType:", litType, " sizeFormat", sizeFormat, "remain:", len(in), "want:", litCompSize)
			return in, ErrBlockTooSmall
		}
		// Store compressed literals, so we defer decoding until we get history.
		literals = in[:litCompSize]
		in = in[litCompSize:]
		if debugDecoder {
			printf("Found %d compressed literals\n", litCompSize)
		}
		huff := hist.huffTree
		if huff == nil {
			return in, errors.New("literal block was treeless, but no history was defined")
		}
		// Ensure we have space to store it.
		if cap(b.literalBuf) < litRegenSize {
			if b.lowMem {
				b.literalBuf = make([]byte, 0, litRegenSize)
			} else {
				b.literalBuf = make([]byte, 0, maxCompressedLiteralSize)
			}
		}
		var err error
		// Use our out buffer.
		if fourStreams {
			literals, err = huff.Decoder().Decompress4X(b.literalBuf[:0:litRegenSize], literals)
		} else {
			literals, err = huff.Decoder().Decompress1X(b.literalBuf[:0:litRegenSize], literals)
		}
		// Make sure we don't leak our literals buffer
		if err != nil {
			println("decompressing literals:", err)
			return in, err
		}
		if len(literals) != litRegenSize {
			return in, fmt.Errorf("literal output size mismatch want %d, got %d", litRegenSize, len(literals))
		}

	case literalsBlockCompressed:
		if len(in) < litCompSize {
			println("too small: litType:", litType, " sizeFormat", sizeFormat, "remain:", len(in), "want:", litCompSize)
			return in, ErrBlockTooSmall
		}
		literals = in[:litCompSize]
		in = in[litCompSize:]
		// Ensure we have space to store it.
		if cap(b.literalBuf) < litRegenSize {
			if b.lowMem {
				b.literalBuf = make([]byte, 0, litRegenSize)
			} else {
				b.literalBuf = make([]byte, 0, maxCompressedLiteralSize)
			}
		}
		huff := hist.huffTree
		if huff == nil {
			huff = huffDecoderPool.Get().(*huff0.Scratch)
			if huff == nil {
				huff = &huff0.Scratch{}
			}
		}
		var err error
		huff, literals, err = huff0.ReadTable(literals, huff)
		if err != nil {
			println("reading huffman table:", err)
			return in, err
		}
		hist.huffTree = huff
		// Use our out buffer.
		if fourStreams {
			literals, err = huff.Decoder().Decompress4X(b.literalBuf[:0:litRegenSize], literals)
		} else {
			literals, err = huff.Decoder().Decompress1X(b.literalBuf[:0:litRegenSize], literals)
		}
		if err != nil {
			println("decoding compressed literals:", err)
			return in, err
		}
		// Make sure we don't leak our literals buffer
		if len(literals) != litRegenSize {
			return in, fmt.Errorf("literal output size mismatch want %d, got %d", litRegenSize, len(literals))
		}
		if debugDecoder {
			printf("Decompressed %d literals into %d bytes\n", litCompSize, litRegenSize)
		}
	}
	hist.decoders.literals = literals
	return in, nil
}

// decodeCompressed will start decompressing a block.
func (b *blockDec) decodeCompressed(hist *history) error {
	in := b.data
	in, err := b.decodeLiterals(in, hist)
	if err != nil {
		return err
	}
	err = b.prepareSequences(in, hist)
	if err != nil {
		return err
	}
	err = hist.decoders.decodeSync(hist.b)
	if err != nil {
		return err
	}
	return b.updateHistory(hist)
}

func (b *blockDec) prepareSequences(in []byte, hist *history) (err error) {
	// Decode Sequences
	// https://github.com/facebook/zstd/blob/dev/doc/zstd_compression_format.md#sequences-section
	if len(in) < 1 {
		return ErrBlockTooSmall
	}
	var nSeqs int
	seqHeader := in[0]
	switch {
	case seqHeader == 0:
		in = in[1:]
	case seqHeader < 128:
		nSeqs = int(seqHeader)
		in = in[1:]
	case seqHeader < 255:
		if len(in) < 2 {
			return ErrBlockTooSmall
		}
		nSeqs = int(seqHeader-128)<<8 | int(in[1])
		in = in[2:]
	case seqHeader == 255:
		if len(in) < 3 {
			return ErrBlockTooSmall
		}
		nSeqs = 0x7f00 + int(in[1]) + (int(in[2]) << 8)
		in = in[3:]
	}

	var seqs = &hist.decoders
	seqs.nSeqs = nSeqs
	if nSeqs > 0 {
		if len(in) < 1 {
			return ErrBlockTooSmall
		}
		br := byteReader{b: in, off: 0}
		compMode := br.Uint8()
		br.advance(1)
		if debugDecoder {
			printf("Compression modes: 0b%b", compMode)
		}
		for i := uint(0); i < 3; i++ {
			mode := seqCompMode((compMode >> (6 - i*2)) & 3)
			if debugDecoder {
				println("Table", tableIndex(i), "is", mode)
			}
			var seq *sequenceDec
			switch tableIndex(i) {
			case tableLiteralLengths:
				seq = &seqs.litLengths
			case tableOffsets:
				seq = &seqs.offsets
			case tableMatchLengths:
				seq = &seqs.matchLengths
			default:
				panic("unknown table")
			}
			switch mode {
			case compModePredefined:
				seq.fse = &fsePredef[i]
			case compModeRLE:
				if br.remain() < 1 {
					return ErrBlockTooSmall
				}
				v := br.Uint8()
				br.advance(1)
				dec := fseDecoderPool.Get().(*fseDecoder)
				symb, err := decSymbolValue(v, symbolTableX[i])
				if err != nil {
					printf("RLE Transform table (%v) error: %v", tableIndex(i), err)
					return err
				}
				dec.setRLE(symb)
				seq.fse = dec
				if debugDecoder {
					printf("RLE set to %+v, code: %v", symb, v)
				}
			case compModeFSE:
				println("Reading table for", tableIndex(i))
				dec := fseDecoderPool.Get().(*fseDecoder)
				err := dec.readNCount(&br, uint16(maxTableSymbol[i]))
				if err != nil {
					println("Read table error:", err)
					return err
				}
				err = dec.transform(symbolTableX[i])
				if err != nil {
					println("Transform table error:", err)
					return err
				}
				if debugDecoder {
					println("Read table ok", "symbolLen:", dec.symbolLen)
				}
				seq.fse = dec
			case compModeRepeat:
				seq.repeat = true
			}
			if br.overread() {
				return io.ErrUnexpectedEOF
			}
		}
		in = br.unread()
	}
	if debugDecoder {
		println("Literals:", len(seqs.literals), "hash:", xxhash.Sum64(seqs.literals), "and", seqs.nSeqs, "sequences.")
	}

	if nSeqs == 0 {
		return nil
	}
	br := &bitReader{}
	if err := br.init(in); err != nil {
		return err
	}

	if err := seqs.initialize(br, hist, b.dst); err != nil {
		println("initializing sequences:", err)
		return err
	}
	return nil
}

func (b *blockDec) decodeSequences(hist *history) error {
	return hist.decoders.decode()
}

func (b *blockDec) executeSequences(hist *history) error {
	hbytes := hist.b
	if len(hbytes) > hist.windowSize {
		hbytes = hbytes[len(hbytes)-hist.windowSize:]
		// We do not need history anymore.
		if hist.dict != nil {
			hist.dict.content = nil
		}
	}
	err := hist.decoders.execute(hist.b)
	if err != nil {
		return err
	}
	return b.updateHistory(hist)
}

func (b *blockDec) updateHistory(hist *history) error {
	if len(b.data) > maxCompressedBlockSize {
		return fmt.Errorf("compressed block size too large (%d)", len(b.data))
	}
	// Set output and release references.
	b.dst = hist.decoders.out
	hist.decoders.out, hist.decoders.literals = nil, nil

	hist.recentOffsets = hist.decoders.prevOffset
	if b.Last {
		// if last block we don't care about history.
		println("Last block, no history returned")
		hist.b = hist.b[:0]
		return nil
	}
	hist.append(b.dst)
	if debugDecoder {
		println("Finished block with literals:", len(hist.decoders.literals), "and", len(hist.decoders.seq), "sequences.")
	}
	return nil
}
