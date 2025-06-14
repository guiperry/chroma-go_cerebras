package defaultef

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	ort "github.com/yalue/onnxruntime_go"

	"github.com/guiperry/chroma-go_cerebras/pkg/embeddings"
	tokenizers "github.com/guiperry/chroma-go_cerebras/pkg/tokenizers/libtokenizers"
)

type Option func(p *DefaultEmbeddingFunction) error

var _ embeddings.EmbeddingFunction = (*DefaultEmbeddingFunction)(nil)

type DefaultEmbeddingFunction struct {
	tokenizer *tokenizers.Tokenizer
	closed    int32
	closeOnce sync.Once
}

var (
	initLock sync.Mutex
	arc      = &AtomicRefCounter{} // even with arc it is possible that someone calls ort.DestroyEnvironment() from outside, so this is not great, we need a better abstraction than this
)

func NewDefaultEmbeddingFunction(opts ...Option) (*DefaultEmbeddingFunction, func() error, error) {
	initLock.Lock()
	defer initLock.Unlock()
	err := EnsureLibTokenizersSharedLibrary()
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to ensure lib tokenizers")
	}
	err = EnsureOnnxRuntimeSharedLibrary()
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to ensure onnx runtime shared library")
	}
	err = EnsureDefaultEmbeddingFunctionModel()
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to ensure default embedding function model")
	}
	err = tokenizers.LoadLibrary(libTokenizersLibPath)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to load libtokenizers shared library")
	}
	updatedConfigBytes, err := updateConfig(onnxModelTokenizerConfigPath)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to update tokenizer config")
	}
	tk, err := tokenizers.FromBytes(updatedConfigBytes)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create tokenizer from bytes")
	}
	ef := &DefaultEmbeddingFunction{tokenizer: tk}
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(onnxLibPath)
		err = ort.InitializeEnvironment()
		if err != nil {
			errc := ef.Close()
			if errc != nil {
				fmt.Printf("error while closing embedding function %v", errc.Error())
			}
			return nil, nil, err
		}
	}
	arc.Increment()

	return ef, ef.Close, nil
}

type EmbeddingInput struct {
	shape           *ort.Shape
	inputTensor     *ort.Tensor[int64]
	attentionTensor *ort.Tensor[int64]
	typeIDSTensor   *ort.Tensor[int64]
}

func NewEmbeddingInput(inputIDs []int64, attnMask []int64, typeIDs []int64, numInputs, vlen int64) (*EmbeddingInput, error) {
	inputShape := ort.NewShape(numInputs, vlen)
	inputTensor, err := ort.NewTensor(inputShape, inputIDs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create input tensor")
	}
	attentionTensor, err := ort.NewTensor(inputShape, attnMask)
	if err != nil {
		derr := inputTensor.Destroy()
		if derr != nil {
			return nil, errors.Wrapf(err, "failed to create attention mask tensor. destroyed input tensor %v (potential memory leak)", derr)
		}
		return nil, errors.Wrap(err, "failed to create attention mask tensor")
	}
	typeTensor, err := ort.NewTensor(inputShape, typeIDs)
	if err != nil {
		derr := inputTensor.Destroy()
		if derr != nil {
			fmt.Printf("potential memory leak. Failed to destroy input tensor %e", derr)
		}
		derr = attentionTensor.Destroy()
		if derr != nil {
			fmt.Printf("potential memory leak. Failed to destroy attention tensor %e", derr)
		}
		return nil, errors.Wrap(err, "failed to create type IDs tensor")
	}
	return &EmbeddingInput{
		shape:           &inputShape,
		inputTensor:     inputTensor,
		attentionTensor: attentionTensor,
		typeIDSTensor:   typeTensor,
	}, nil
}

func (ei *EmbeddingInput) Close() error {
	var errOut []error
	err1 := ei.inputTensor.Destroy()
	if err1 != nil {
		errOut = append(errOut, err1)
	}
	err2 := ei.attentionTensor.Destroy()
	if err2 != nil {
		errOut = append(errOut, err2)
	}

	err3 := ei.typeIDSTensor.Destroy()
	if err3 != nil {
		errOut = append(errOut, err3)
	}
	if len(errOut) > 0 {
		return errors.Errorf("errors: %v", errOut)
	}
	return nil
}

func (e *DefaultEmbeddingFunction) tokenize(documents []string) (*EmbeddingInput, error) {
	var tensorSize int64 = 0
	var numInputs = int64(len(documents))
	var vlen int64 = 0
	inputIDs := make([]int64, tensorSize)
	attnMask := make([]int64, tensorSize)
	typeIDs := make([]int64, tensorSize)
	for _, doc := range documents {
		res1, err := e.tokenizer.EncodeWithOptions(doc, true, tokenizers.WithReturnAttentionMask(), tokenizers.WithReturnTypeIDs())
		if err != nil {
			return nil, err
		}
		for i := range res1.IDs {
			inputIDs = append(inputIDs, int64(res1.IDs[i]))
			attnMask = append(attnMask, int64(res1.AttentionMask[i]))
			typeIDs = append(typeIDs, int64(res1.TypeIDs[i]))
		}
		vlen = int64(math.Max(float64(vlen), float64(len(res1.IDs))))
		tensorSize += int64(len(res1.IDs))
	}
	return NewEmbeddingInput(inputIDs, attnMask, typeIDs, numInputs, vlen)
}

func (e *DefaultEmbeddingFunction) encode(embeddingInput *EmbeddingInput) ([]embeddings.Embedding, error) {
	outputShape := ort.NewShape(append(*embeddingInput.shape, 384)...)
	shapeInt32 := make([]int, len(outputShape))

	for i, v := range outputShape {
		shapeInt32[i] = int(v)
	}
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, err
	}
	defer func(outputTensor *ort.Tensor[float32]) {
		err := outputTensor.Destroy()
		if err != nil {
			fmt.Printf("potential memory leak. Failed to destory outputTensor %v", err)
		}
	}(outputTensor)
	session, err := ort.NewAdvancedSession(onnxModelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"},
		[]ort.Value{embeddingInput.inputTensor, embeddingInput.attentionTensor, embeddingInput.typeIDSTensor}, []ort.Value{outputTensor}, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create session")
	}
	defer func(session *ort.AdvancedSession) {
		err := session.Destroy()
		if err != nil {
			fmt.Printf("potential memory leak. Failed to destory ORT session %v", err)
		}
	}(session)

	err = session.Run()
	if err != nil {
		return nil, errors.Wrap(err, "failed to run session")
	}
	outputData := outputTensor.GetData()
	t, err := ReshapeFlattenedTensor(outputData, shapeInt32)
	if err != nil {
		return nil, errors.Wrap(err, "failed to reshape flattened tensor")
	}

	expandedDims, err := ExpandDims(embeddingInput.attentionTensor.GetData(), *embeddingInput.shape)
	if err != nil {
		return nil, errors.Wrap(err, "failed to expand dimensions of attention mask")
	}
	expandedMask := BroadcastTo(expandedDims, [3]int(shapeInt32))
	mtpl, err := multiply(t.(Tensor3D[float32]), expandedMask)
	if err != nil {
		return nil, errors.Wrap(err, "failed to multiply tensor")
	}

	summed, err := mtpl.Sum(1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to sum tensor")
	}
	summedExpandedMask, err := expandedMask.Sum(1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to expanded mask")
	}
	summedExpandedMaskF32 := ConvertTensor2D[int64, float32](summedExpandedMask)
	clippedSummed := clip(summedExpandedMaskF32, 1e-9, math.MaxFloat32)
	emb := divide(summed, clippedSummed)
	normalizedEmbeddings := normalize(emb)
	return embeddings.NewEmbeddingsFromFloat32(normalizedEmbeddings)
}

func (e *DefaultEmbeddingFunction) EmbedDocuments(ctx context.Context, documents []string) ([]embeddings.Embedding, error) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, errors.New("embedding function is closed")
	}
	embeddingInputs, err := e.tokenize(documents)
	if err != nil {
		return nil, errors.Wrap(err, "failed to tokenize documents")
	}
	defer func() {
		if err := embeddingInputs.Close(); err != nil {
			fmt.Printf("failed to close embedding input %v", err.Error())
		}
	}()
	ebmds, err := e.encode(embeddingInputs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to embed documents")
	}
	if len(ebmds) == 0 {
		return embeddings.NewEmptyEmbeddings(), nil
	}
	if len(ebmds) != len(documents) {
		return nil, errors.Errorf("number of embeddings %d does not match number of documents %d", len(ebmds), len(documents))
	}
	return ebmds, nil
}

func (e *DefaultEmbeddingFunction) EmbedQuery(ctx context.Context, document string) (embeddings.Embedding, error) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, errors.New("embedding function is closed")
	}
	embeddingInputs, err := e.tokenize([]string{document})
	if err != nil {
		return nil, errors.Wrap(err, "failed to tokenize query")
	}
	embds, err := e.encode(embeddingInputs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode query")
	}
	defer func() {
		if err := embeddingInputs.Close(); err != nil {
			fmt.Printf("failed to close embedding input %v", err.Error())
		}
	}()
	if len(embds) == 0 {
		return embeddings.NewEmptyEmbedding(), nil
	}

	return embds[0], nil
}

// func (e *DefaultEmbeddingFunction) EmbedRecords(ctx context.Context, records []v2.Record, force bool) error {
//	if atomic.LoadInt32(&e.closed) == 1 {
//		return fmt.Errorf("embedding function is closed")
//	}
//	return embeddings.EmbedRecordsDefaultImpl(e, ctx, records, force)
//}

func updateConfig(filename string) ([]byte, error) {
	// Read the file
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, errors.Wrap(err, "error reading file")
	}

	// Unmarshal JSON into a map
	var jsonMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &jsonMap); err != nil {
		return nil, errors.Wrap(err, "error unmarshaling JSON")
	}

	// Update truncation.max_length
	if truncation, ok := jsonMap["truncation"]; ok {
		var truncationMap map[string]interface{}
		if err := json.Unmarshal(truncation, &truncationMap); err != nil {
			return nil, errors.Wrap(err, "error unmarshaling truncation")
		}
		truncationMap["max_length"] = 256
		updatedTruncation, err := json.Marshal(truncationMap)
		if err != nil {
			return nil, errors.Wrap(err, "error marshaling updated truncation")
		}
		jsonMap["truncation"] = updatedTruncation
	}

	// Update padding.strategy.Fixed
	if padding, ok := jsonMap["padding"]; ok {
		var paddingMap map[string]json.RawMessage
		if err := json.Unmarshal(padding, &paddingMap); err != nil {
			return nil, errors.Wrap(err, "error unmarshaling padding")
		}
		if strategy, ok := paddingMap["strategy"]; ok {
			var strategyMap map[string]int
			if err := json.Unmarshal(strategy, &strategyMap); err != nil {
				return nil, errors.Wrap(err, "error unmarshaling strategy")
			}
			strategyMap["Fixed"] = 256
			updatedStrategy, err := json.Marshal(strategyMap)
			if err != nil {
				return nil, errors.Wrap(err, "error marshaling updated strategy")
			}
			paddingMap["strategy"] = updatedStrategy
		}
		updatedPadding, err := json.Marshal(paddingMap)
		if err != nil {
			return nil, errors.Wrap(err, "error marshaling updated padding")
		}
		jsonMap["padding"] = updatedPadding
	}

	// Marshal the updated map back to JSON
	updatedData, err := json.MarshalIndent(jsonMap, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling updated JSON")
	}

	return updatedData, nil
}

func (e *DefaultEmbeddingFunction) Close() error {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil
	}
	arc.Decrement()
	var closeErr error
	if arc.GetCount() == 0 {
		e.closeOnce.Do(func() {
			var errs []error
			if e.tokenizer != nil {
				err := e.tokenizer.Close()
				if err != nil {
					errs = append(errs, err)
				}
			}
			if ort.IsInitialized() { // skip destroying the environment if it is not initialized
				err := ort.DestroyEnvironment()
				if err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) > 0 {
				closeErr = errors.Errorf("errors: %v", errs)
			}
			atomic.StoreInt32(&e.closed, 1)
		})
	}
	return closeErr
}

type AtomicRefCounter struct {
	count int32
}

func (arc *AtomicRefCounter) Increment() {
	atomic.AddInt32(&arc.count, 1)
}

func (arc *AtomicRefCounter) Decrement() {
	if atomic.LoadInt32(&arc.count) == 0 {
		return
	}
	atomic.AddInt32(&arc.count, -1)
}

func (arc *AtomicRefCounter) GetCount() int32 {
	return atomic.LoadInt32(&arc.count)
}
