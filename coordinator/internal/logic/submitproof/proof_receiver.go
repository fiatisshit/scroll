package submitproof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"scroll-tech/common/version"
	"time"

	"scroll-tech/coordinator/internal/logic/old_verifier"

	jwt "github.com/appleboy/gin-jwt/v2"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/scroll-tech/go-ethereum/log"
	"gorm.io/gorm"

	"scroll-tech/common/types"
	"scroll-tech/common/types/message"

	"scroll-tech/coordinator/internal/config"
	"scroll-tech/coordinator/internal/logic/verifier"
	"scroll-tech/coordinator/internal/orm"
	coordinatorType "scroll-tech/coordinator/internal/types"
)

var (
	// ErrValidatorFailureProofMsgStatusNotOk proof msg status not ok
	ErrValidatorFailureProofMsgStatusNotOk = errors.New("validator failure proof msg status not ok")
	// ErrValidatorFailureProverTaskEmpty get none prover task
	ErrValidatorFailureProverTaskEmpty = errors.New("validator failure get none prover task for the proof")
	// ErrInvalidProverVersion means prover version is invalid.
	ErrInvalidProverVersion = errors.New("prover version invalid")
	// ErrValidatorFailureProverTaskCannotSubmitTwice prove task can not submit proof twice
	ErrValidatorFailureProverTaskCannotSubmitTwice = errors.New("validator failure prove task cannot submit proof twice")
	// ErrValidatorFailureProofTimeout the submit proof is timeout
	ErrValidatorFailureProofTimeout = errors.New("validator failure submit proof timeout")
	// ErrValidatorFailureTaskHaveVerifiedSuccess have proved success and verified success
	ErrValidatorFailureTaskHaveVerifiedSuccess = errors.New("validator failure chunk/batch have proved and verified success")
)

// ProofReceiverLogic the proof receiver logic
type ProofReceiverLogic struct {
	chunkOrm      *orm.Chunk
	batchOrm      *orm.Batch
	proverTaskOrm *orm.ProverTask

	db  *gorm.DB
	cfg *config.ProverManager

	verifier    *verifier.Verifier
	oldVerifier *old_verifier.OldVerifier

	proofReceivedTotal                    prometheus.Counter
	proofSubmitFailure                    prometheus.Counter
	verifierTotal                         *prometheus.CounterVec
	verifierFailureTotal                  *prometheus.CounterVec
	proverTaskProveDuration               prometheus.Histogram
	validateFailureTotal                  prometheus.Counter
	validateFailureProverTaskSubmitTwice  prometheus.Counter
	validateFailureProverTaskStatusNotOk  prometheus.Counter
	validateFailureProverTaskTimeout      prometheus.Counter
	validateFailureProverTaskHaveVerifier prometheus.Counter
}

// NewSubmitProofReceiverLogic create a proof receiver logic
func NewSubmitProofReceiverLogic(cfg *config.ProverManager, db *gorm.DB, reg prometheus.Registerer) *ProofReceiverLogic {
	vf, err := verifier.NewVerifier(cfg.Verifier)
	if err != nil {
		panic("proof receiver new verifier failure")
	}
	oldVf, err := old_verifier.NewOldVerifier(cfg.OldVerifier)
	if err != nil {
		panic("proof receiver new OldVerifier failure")
	}
	return &ProofReceiverLogic{
		chunkOrm:      orm.NewChunk(db),
		batchOrm:      orm.NewBatch(db),
		proverTaskOrm: orm.NewProverTask(db),

		cfg: cfg,
		db:  db,

		verifier:    vf,
		oldVerifier: oldVf,

		proofReceivedTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_submit_proof_total",
			Help: "Total number of submit proof.",
		}),
		proofSubmitFailure: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_submit_proof_failure_total",
			Help: "Total number of submit proof failure.",
		}),
		verifierTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "coordinator_verifier_total",
			Help: "Total number of verifier.",
		}, []string{"version"}),
		verifierFailureTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "coordinator_verifier_failure_total",
			Help: "Total number of verifier failure.",
		}, []string{"version"}),
		proverTaskProveDuration: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
			Name:    "coordinator_task_prove_duration_seconds",
			Help:    "Time spend by prover prove task.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60},
		}),
		validateFailureTotal: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_validate_failure_total",
			Help: "Total number of submit proof validate failure.",
		}),
		validateFailureProverTaskSubmitTwice: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_validate_failure_submit_twice_total",
			Help: "Total number of submit proof validate failure submit twice.",
		}),
		validateFailureProverTaskStatusNotOk: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_validate_failure_submit_status_not_ok",
			Help: "Total number of submit proof validate failure proof status not ok.",
		}),
		validateFailureProverTaskTimeout: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_validate_failure_submit_timeout",
			Help: "Total number of submit proof validate failure timeout.",
		}),
		validateFailureProverTaskHaveVerifier: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "coordinator_validate_failure_submit_have_been_verifier",
			Help: "Total number of submit proof validate failure proof have been verifier.",
		}),
	}
}

// HandleZkProof handle a ZkProof submitted from a prover.
// For now only proving/verifying error will lead to setting status as skipped.
// db/unmarshal errors will not because they are errors on the business logic side.
func (m *ProofReceiverLogic) HandleZkProof(ctx *gin.Context, proofMsg *message.ProofMsg) error {
	m.proofReceivedTotal.Inc()
	pk := ctx.GetString(coordinatorType.PublicKey)
	if len(pk) == 0 {
		return fmt.Errorf("get public key from context failed")
	}

	proverTask, err := m.proverTaskOrm.GetProverTaskByTaskIDAndPubKey(ctx, proofMsg.ID, pk)
	if proverTask == nil || err != nil {
		log.Error("get none prover task for the proof", "key", pk, "taskID", proofMsg.ID, "error", err)
		return ErrValidatorFailureProverTaskEmpty
	}

	proofTime := time.Since(proverTask.CreatedAt)
	proofTimeSec := uint64(proofTime.Seconds())

	log.Info("handling zk proof", "proof id", proofMsg.ID, "prover name", proverTask.ProverName,
		"prover pk", pk, "prove type", proverTask.TaskType, "proof time", proofTimeSec)

	if err = m.validator(ctx, proverTask, pk, proofMsg); err != nil {
		return err
	}

	proverVersion := ctx.GetString(coordinatorType.ProverVersion)
	m.verifierTotal.WithLabelValues(proverVersion).Inc()

	var success bool
	var verifyErr error
	if proofMsg.Type == message.ProofTypeChunk {
		success, verifyErr = m.verifyChunkProof(ctx, proofMsg.ChunkProof)
	} else if proofMsg.Type == message.ProofTypeBatch {
		success, verifyErr = m.verifyBatchProof(ctx, proofMsg.BatchProof)
	}

	if verifyErr != nil || !success {
		m.verifierFailureTotal.WithLabelValues(proverVersion).Inc()
		m.proofFailure(ctx, proofMsg.ID, pk, proofMsg)

		log.Info("proof verified by coordinator failed", "proof id", proofMsg.ID, "prover name", proverTask.ProverName,
			"prover pk", pk, "prove type", proofMsg.Type, "proof time", proofTimeSec, "error", verifyErr)

		if verifyErr == nil {
			verifyErr = fmt.Errorf("verification succeeded and it's an invalid proof")
		}
		return verifyErr
	}

	m.proverTaskProveDuration.Observe(time.Since(proverTask.CreatedAt).Seconds())

	log.Info("proof verified and valid", "proof id", proofMsg.ID, "prover name", proverTask.ProverName,
		"prover pk", pk, "prove type", proofMsg.Type, "proof time", proofTimeSec)

	if err := m.closeProofTask(ctx, proofMsg.ID, pk, proofMsg, proofTimeSec); err != nil {
		m.proofSubmitFailure.Inc()
		m.proofRecover(ctx, proofMsg.ID, pk, proofMsg)
		return err
	}

	return nil
}

func (m *ProofReceiverLogic) checkAreAllChunkProofsReady(ctx context.Context, chunkHash string) error {
	batchHash, err := m.chunkOrm.GetChunkBatchHash(ctx, chunkHash)
	if err != nil {
		return err
	}

	allReady, err := m.chunkOrm.CheckIfBatchChunkProofsAreReady(ctx, batchHash)
	if err != nil {
		return err
	}
	if allReady {
		err := m.batchOrm.UpdateChunkProofsStatusByBatchHash(ctx, batchHash, types.ChunkProofsStatusReady)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *ProofReceiverLogic) validator(ctx context.Context, proverTask *orm.ProverTask, pk string, proofMsg *message.ProofMsg) error {
	m.validateFailureTotal.Inc()
	// Ensure this prover is eligible to participate in the prover task.
	if types.ProverProveStatus(proverTask.ProvingStatus) == types.ProverProofValid {
		m.validateFailureProverTaskSubmitTwice.Inc()
		// In order to prevent DoS attacks, it is forbidden to repeatedly submit valid proofs.
		// TODO: Defend invalid proof resubmissions by one of the following two methods:
		// (i) slash the prover for each submission of invalid proof
		// (ii) set the maximum failure retry times
		log.Warn("the prover task cannot submit twice", "hash", proofMsg.ID, "prover pk", proverTask.ProverPublicKey,
			"prover name", proverTask.ProverName, "proof type", proverTask.TaskType)
		return ErrValidatorFailureProverTaskCannotSubmitTwice
	}

	proofTime := time.Since(proverTask.CreatedAt)
	proofTimeSec := uint64(proofTime.Seconds())

	if proofMsg.Status != message.StatusOk {
		m.validateFailureProverTaskStatusNotOk.Inc()
		log.Info("proof generated by prover failed", "proof id", proofMsg.ID, "prover name", proverTask.ProverName,
			"prover pk", pk, "prove type", proofMsg.Type, "error", proofMsg.Error)

		if updateErr := m.proverTaskOrm.UpdateProverTaskProvingStatus(ctx, proofMsg.Type, proofMsg.ID, pk, types.ProverProofInvalid); updateErr != nil {
			log.Error("proof generated by prover failed update prover task proving status failure", "proof id", proofMsg.ID,
				"prover name", proverTask.ProverName, "prover pk", pk, "prove type", proofMsg.Type, "error", proofMsg.Error)
		}
		return ErrValidatorFailureProofMsgStatusNotOk
	}

	// if prover task FailureType is SessionInfoFailureTimeout, the submit proof is timeout, need skip it
	if types.ProverTaskFailureType(proverTask.FailureType) == types.ProverTaskFailureTypeTimeout {
		m.validateFailureProverTaskTimeout.Inc()
		log.Info("proof submit proof have timeout, skip this submit proof", "hash", proofMsg.ID, "proof type", proverTask.TaskType,
			"prover name", proverTask.ProverName, "prover public key", pk, "proof time", proofTimeSec)
		return ErrValidatorFailureProofTimeout
	}

	// store the proof to prover task
	if updateTaskProofErr := m.updateProverTaskProof(ctx, pk, proofMsg); updateTaskProofErr != nil {
		log.Warn("update prover task proof failure", "hash", proofMsg.ID, "public key", pk,
			"prover name", proverTask.ProverName, "error", updateTaskProofErr)
	}

	// if the batch/chunk have proved and verifier success, need skip this submit proof
	if m.checkIsTaskSuccess(ctx, proofMsg.ID, proofMsg.Type) {
		m.validateFailureProverTaskHaveVerifier.Inc()
		log.Info("the prove task have proved and verifier success, skip this submit proof", "hash", proofMsg.ID,
			"proof type", proverTask.TaskType, "prover name", proverTask.ProverName, "prover public key", pk)
		return ErrValidatorFailureTaskHaveVerifiedSuccess
	}
	return nil
}

func (m *ProofReceiverLogic) proofFailure(ctx context.Context, hash string, pubKey string, proofMsg *message.ProofMsg) {
	log.Info("proof failure update proof status", "hash", hash, "public key", pubKey,
		"proof type", proofMsg.Type.String(), "status", types.ProvingTaskFailed.String())

	if err := m.updateProofStatus(ctx, hash, pubKey, proofMsg, types.ProvingTaskFailed, 0); err != nil {
		log.Error("failed to updated proof status ProvingTaskFailed", "hash", hash, "pubKey", pubKey, "error", err)
	}
}

func (m *ProofReceiverLogic) proofRecover(ctx context.Context, hash string, pubKey string, proofMsg *message.ProofMsg) {
	log.Info("proof recover update proof status", "hash", hash, "public key", pubKey,
		"proof type", proofMsg.Type.String(), "status", types.ProvingTaskUnassigned.String())

	if err := m.updateProofStatus(ctx, hash, pubKey, proofMsg, types.ProvingTaskUnassigned, 0); err != nil {
		log.Error("failed to updated proof status ProvingTaskUnassigned", "hash", hash, "pubKey", pubKey, "error", err)
	}
}

func (m *ProofReceiverLogic) closeProofTask(ctx context.Context, hash string, pubKey string, proofMsg *message.ProofMsg, proofTimeSec uint64) error {
	log.Info("proof close task update proof status", "hash", hash, "public key", pubKey,
		"proof type", proofMsg.Type.String(), "status", types.ProvingTaskVerified.String())

	if err := m.updateProofStatus(ctx, hash, pubKey, proofMsg, types.ProvingTaskVerified, proofTimeSec); err != nil {
		log.Error("failed to updated proof status ProvingTaskVerified", "hash", hash, "pubKey", pubKey, "error", err)
		return err
	}
	return nil
}

// UpdateProofStatus update the chunk/batch task and session info status
func (m *ProofReceiverLogic) updateProofStatus(ctx context.Context, hash string, proverPublicKey string, proofMsg *message.ProofMsg, status types.ProvingStatus, proofTimeSec uint64) error {
	var proverTaskStatus types.ProverProveStatus
	switch status {
	case types.ProvingTaskFailed, types.ProvingTaskUnassigned:
		proverTaskStatus = types.ProverProofInvalid
	case types.ProvingTaskVerified:
		proverTaskStatus = types.ProverProofValid
	}

	err := m.db.Transaction(func(tx *gorm.DB) error {
		if updateErr := m.proverTaskOrm.UpdateProverTaskProvingStatus(ctx, proofMsg.Type, hash, proverPublicKey, proverTaskStatus, tx); updateErr != nil {
			return updateErr
		}

		// if the block batch has proof verified, so the failed status not update block batch proving status
		if status == types.ProvingTaskFailed && m.checkIsTaskSuccess(ctx, hash, proofMsg.Type) {
			log.Info("update proof status ProvingTaskFailed skip because other prover have prove success", "hash", hash, "public key", proverPublicKey)
			return nil
		}

		if status == types.ProvingTaskVerified {
			var storeProofErr error
			switch proofMsg.Type {
			case message.ProofTypeChunk:
				storeProofErr = m.chunkOrm.UpdateProofByHash(ctx, proofMsg.ID, proofMsg.ChunkProof, proofTimeSec, tx)
			case message.ProofTypeBatch:
				storeProofErr = m.batchOrm.UpdateProofByHash(ctx, proofMsg.ID, proofMsg.BatchProof, proofTimeSec, tx)
			}
			if storeProofErr != nil {
				log.Error("failed to store chunk/batch proof into db", "hash", hash, "public key", proverPublicKey, "error", storeProofErr)
				return storeProofErr
			}
		}

		switch proofMsg.Type {
		case message.ProofTypeChunk:
			if err := m.chunkOrm.UpdateProvingStatus(ctx, hash, status, tx); err != nil {
				log.Error("failed to update chunk proving_status as failed", "msg.ID", hash, "error", err)
				return err
			}
		case message.ProofTypeBatch:
			if err := m.batchOrm.UpdateProvingStatus(ctx, hash, status, tx); err != nil {
				log.Error("failed to update batch proving_status as failed", "msg.ID", hash, "error", err)
				return err
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	if status == types.ProvingTaskVerified && proofMsg.Type == message.ProofTypeChunk {
		if checkReadyErr := m.checkAreAllChunkProofsReady(ctx, hash); checkReadyErr != nil {
			log.Error("failed to check are all chunk proofs ready", "error", checkReadyErr)
			return checkReadyErr
		}
	}

	return nil
}

func (m *ProofReceiverLogic) checkIsTaskSuccess(ctx context.Context, hash string, proofType message.ProofType) bool {
	var provingStatus types.ProvingStatus
	var err error

	switch proofType {
	case message.ProofTypeChunk:
		provingStatus, err = m.chunkOrm.GetProvingStatusByHash(ctx, hash)
		if err != nil {
			return false
		}
	case message.ProofTypeBatch:
		provingStatus, err = m.batchOrm.GetProvingStatusByHash(ctx, hash)
		if err != nil {
			return false
		}
	}

	return provingStatus == types.ProvingTaskVerified
}

func (m *ProofReceiverLogic) updateProverTaskProof(ctx context.Context, pk string, proofMsg *message.ProofMsg) error {
	// store the proof to prover task
	var proofBytes []byte
	var marshalErr error
	switch proofMsg.Type {
	case message.ProofTypeChunk:
		proofBytes, marshalErr = json.Marshal(proofMsg.ChunkProof)
	case message.ProofTypeBatch:
		proofBytes, marshalErr = json.Marshal(proofMsg.BatchProof)
	}

	if len(proofBytes) == 0 || marshalErr != nil {
		return fmt.Errorf("updateProverTaskProof marshal proof error:%w", marshalErr)
	}
	return m.proverTaskOrm.UpdateProverTaskProof(ctx, proofMsg.Type, proofMsg.ID, pk, proofBytes)
}

func (m *ProofReceiverLogic) verifyChunkProof(c *gin.Context, proof *message.ChunkProof) (bool, error) {
	claims := jwt.ExtractClaims(c)
	proverVersion := claims[coordinatorType.ProverVersion]
	switch proverVersion.(string) {
	case version.Version:
		return m.verifier.VerifyChunkProof(proof)
	case version.OldVersion:
		return m.oldVerifier.VerifyChunkProof(proof)
	}
	return false, ErrInvalidProverVersion
}

func (m *ProofReceiverLogic) verifyBatchProof(c *gin.Context, proof *message.BatchProof) (bool, error) {
	claims := jwt.ExtractClaims(c)
	proverVersion := claims[coordinatorType.ProverVersion]
	switch proverVersion.(string) {
	case version.Version:
		return m.verifier.VerifyBatchProof(proof)
	case version.OldVersion:
		return m.oldVerifier.VerifyBatchProof(proof)
	}
	return false, ErrInvalidProverVersion
}
