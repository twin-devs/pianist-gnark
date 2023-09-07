// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

// Modifications Copyright 2023 Tianyi Liu and Tiancheng Xie

package gpiano

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"runtime/debug"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/sunblaze-ucb/simpleMPI/mpi"

	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	bn254witness "github.com/consensys/gnark/internal/backend/bn254/witness"

	"github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/logger"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/dkzg"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/kzg"
)

func Verify(proof *Proof, vk *VerifyingKey, publicWitness bn254witness.Witness) error {
	log := logger.Logger().With().Str("curve", "bn254").Str("backend", "gpiano").Logger()
	start := time.Now()

	// pick a hash function to derive the challenge (the same as in the prover)
	hFunc := sha256.New()

	// transcript to derive the challenge
	fs := fiatshamir.NewTranscript(hFunc, "gamma", "etaY", "etaX", "lambda", "alpha", "beta")

	// The first challenge is derived using the public data: the commitments to the permutation,
	// the coefficients of the circuit, and the public inputs.
	// derive gamma from the Comm(blinded cl), Comm(blinded cr), Comm(blinded co)
	if err := bindPublicData(&fs, "gamma", *vk, publicWitness); err != nil {
		return err
	}
	gamma, err := deriveRandomness(&fs, "gamma", true, &proof.LRO[0], &proof.LRO[1], &proof.LRO[2])
	if err != nil {
		return err
	}
	// derive eta from Comm(l), Comm(r), Comm(o)
	etaY, err := deriveRandomness(&fs, "etaY", true)
	if err != nil {
		return err
	}
	etaX, err := deriveRandomness(&fs, "etaX", true)
	if err != nil {
		return err
	}

	// derive lambda from Comm(l), Comm(r), Comm(o), Com(Z)
	lambda, err := deriveRandomness(&fs, "lambda", true, &proof.Z, &proof.W)
	if err != nil {
		return err
	}

	// derive alpha, the point of evaluation
	alpha, err := deriveRandomness(&fs, "alpha", true, &proof.Hx[0], &proof.Hx[1], &proof.Hx[2], &proof.Hx[3])
	if err != nil {
		return err
	}

	// evaluation of Z=Xⁿ⁻¹ at α
	var alphaPowerN, zalpha fr.Element
	var bExpo big.Int
	one := fr.One()
	bExpo.SetUint64(vk.SizeX)
	alphaPowerN.Exp(alpha, &bExpo)
	zalpha.Sub(&alphaPowerN, &one)

	// compute the folded commitment to H: Comm(h₁) + αᵐ*Comm(h₂) + α²⁽ᵐ⁾*Comm(h₃)
	var alphaNBigInt big.Int
	alphaPowerN.ToBigIntRegular(&alphaNBigInt)
	foldedHxDigest := proof.Hx[3]
	foldedHxDigest.ScalarMultiplication(&foldedHxDigest, &alphaNBigInt)
	foldedHxDigest.Add(&foldedHxDigest, &proof.Hx[2])
	foldedHxDigest.ScalarMultiplication(&foldedHxDigest, &alphaNBigInt)
	foldedHxDigest.Add(&foldedHxDigest, &proof.Hx[1])
	foldedHxDigest.ScalarMultiplication(&foldedHxDigest, &alphaNBigInt)
	foldedHxDigest.Add(&foldedHxDigest, &proof.Hx[0])

	foldedPartialProof, foldedPartialDigest, err := dkzg.FoldProof(
		[]dkzg.Digest{
			foldedHxDigest,
			proof.LRO[0],
			proof.LRO[1],
			proof.LRO[2],
			vk.Ql,
			vk.Qr,
			vk.Qm,
			vk.Qo,
			vk.Qk,
			vk.Sy[0],
			vk.Sy[1],
			vk.Sy[2],
			vk.Sx[0],
			vk.Sx[1],
			vk.Sx[2],
			proof.Z,
		},
		&proof.PartialBatchedProof,
		alpha,
		hFunc)

	if err != nil {
		return fmt.Errorf("failed to fold proof on X = alpha: %v", err)
	}
	var shiftedAlpha fr.Element
	shiftedAlpha.Mul(&alpha, &vk.GeneratorX)
	err = dkzg.BatchVerifyMultiPoints(
		[]dkzg.Digest{
			foldedPartialDigest,
			proof.Z,
		},
		[]dkzg.OpeningProof{
			foldedPartialProof,
			proof.PartialZShiftedProof,
		},
		[]fr.Element{
			alpha,
			shiftedAlpha,
		},
		vk.DKZGSRS,
	)
	if err != nil {
		return fmt.Errorf("failed to batch verify on X = alpha: %v", err)
	}

	// derive beta
	ts := []*curve.G1Affine{
		&proof.PartialBatchedProof.H,
	}
	for _, digest := range proof.PartialBatchedProof.ClaimedDigests {
		ts = append(ts, &digest)
	}
	for _, digest := range proof.Hy {
		ts = append(ts, &digest)
	}
	beta, err := deriveRandomness(&fs, "beta", true, ts...)
	if err != nil {
		return err
	}

	if err := checkConstraintY(vk, proof.BatchedProof.ClaimedValues, proof.WShiftedProof.ClaimedValue, etaY, etaX, gamma, lambda, alpha, beta); err != nil {
		return err
	}
	// foldedHy = Hy1 + (beta**M)*Hy2 + (beta**(2M))*Hy3
	var bBetaPowerM, bSize big.Int
	bSize.SetUint64(globalDomain[0].Cardinality)
	var betaPowerM fr.Element
	betaPowerM.Exp(beta, &bSize)
	betaPowerM.ToBigIntRegular(&bBetaPowerM)
	foldedHyDigest := proof.Hy[2]                                      // Hy3
	foldedHyDigest.ScalarMultiplication(&foldedHyDigest, &bBetaPowerM) // (beta**M)*Hy3
	foldedHyDigest.Add(&foldedHyDigest, &proof.Hy[1])                  // (beta**M)*Hy3 + Hy2
	foldedHyDigest.ScalarMultiplication(&foldedHyDigest, &bBetaPowerM) // (beta**(2M))*Hy3 + (beta**M)*Hy2
	foldedHyDigest.Add(&foldedHyDigest, &proof.Hy[0])                  // (beta**(2M))*Hy3 + (beta**M)*Hy2 + Hy1

	foldedProof, foldedDigest, err := kzg.FoldProof(
		append(proof.PartialBatchedProof.ClaimedDigests,
			proof.PartialZShiftedProof.ClaimedDigest,
			proof.W,
			foldedHyDigest,
		),
		&proof.BatchedProof,
		beta,
		hFunc)

	if err != nil {
		return fmt.Errorf("failed to fold proof on X = alpha: %v", err)
	}
	var shiftedBeta fr.Element
	shiftedBeta.Mul(&beta, &vk.GeneratorY)
	err = kzg.BatchVerifyMultiPoints(
		[]kzg.Digest{
			foldedDigest,
			proof.W,
		},
		[]kzg.OpeningProof{
			foldedProof,
			proof.WShiftedProof,
		},
		[]fr.Element{
			beta,
			shiftedBeta,
		},
		vk.KZGSRS,
	)
	if err != nil {
		return fmt.Errorf("failed to batch verify on X = alpha: %v", err)
	}

	log.Debug().Dur("took", time.Since(start)).Msg("verifier done")

	return err
}

// unpack unpacks evaluations from an array
func unpack(src []fr.Element, dst ...*fr.Element) {
	for i := range dst {
		*dst[i] = src[i]
	}
}

func bindPublicData(fs *fiatshamir.Transcript, challenge string, vk VerifyingKey, publicInputs []fr.Element) error {
	// permutation
	if err := fs.Bind(challenge, vk.Sy[0].Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Sy[1].Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Sy[2].Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Sx[0].Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Sx[1].Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Sx[2].Marshal()); err != nil {
		return err
	}

	// coefficients
	if err := fs.Bind(challenge, vk.Ql.Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Qr.Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Qm.Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Qo.Marshal()); err != nil {
		return err
	}
	if err := fs.Bind(challenge, vk.Qk.Marshal()); err != nil {
		return err
	}

	return nil
}

func deriveRandomness(fs *fiatshamir.Transcript, challenge string, notSend bool, points ...*curve.G1Affine) (fr.Element, error) {
	if mpi.SelfRank == 0 {
		var buf [curve.SizeOfG1AffineUncompressed]byte
		var r fr.Element

		for _, p := range points {
			buf = p.RawBytes()
			if err := fs.Bind(challenge, buf[:]); err != nil {
				fmt.Println("deriveRandomness", challenge, "err", err)
				fmt.Println("Stack", string(debug.Stack()))
				return r, err
			}
		}

		b, err := fs.ComputeChallenge(challenge)
		if err != nil {
			fmt.Println("deriveRandomness", challenge, "err", err)
			fmt.Println("Stack", string(debug.Stack()))
			return r, err
		}
		r.SetBytes(b)
		if notSend {
			return r, nil
		}
		sendBuf := r.Bytes()
		for i := 1; i < int(mpi.WorldSize); i++ {
			if err := mpi.SendBytes(sendBuf[:], uint64(i)); err != nil {
				return r, err
			}
		}
		return r, nil
	} else {
		var r fr.Element
		recvBuf, err := mpi.ReceiveBytes(fr.Bytes, 0)
		if err != nil {
			return r, err
		}
		r.SetBytes(recvBuf)
		return r, nil
	}
}

// checkConstraintY checks that the constraint is satisfied
func checkConstraintY(vk *VerifyingKey, evalsYOnBeta []fr.Element, ws, etaY, etaX, gamma, lambda, alpha, beta fr.Element) error {
	// unpack vector evalsXOnAlpha on l, r, o, ql, qr, qm, qo, qk, s1, s2, s3, z, zmu
	hx := evalsYOnBeta[0]
	l := evalsYOnBeta[1]
	r := evalsYOnBeta[2]
	o := evalsYOnBeta[3]
	ql := evalsYOnBeta[4]
	qr := evalsYOnBeta[5]
	qm := evalsYOnBeta[6]
	qo := evalsYOnBeta[7]
	qk := evalsYOnBeta[8]
	sy1 := evalsYOnBeta[9]
	sy2 := evalsYOnBeta[10]
	sy3 := evalsYOnBeta[11]
	sx1 := evalsYOnBeta[12]
	sx2 := evalsYOnBeta[13]
	sx3 := evalsYOnBeta[14]
	z := evalsYOnBeta[15]
	zs := evalsYOnBeta[16]
	w := evalsYOnBeta[17]
	hy := evalsYOnBeta[18]
	// first part: individual constraints
	var firstPart fr.Element
	ql.Mul(&ql, &l)
	qr.Mul(&qr, &r)
	qm.Mul(&qm, &l).Mul(&qm, &r)
	qo.Mul(&qo, &o)
	firstPart.Add(&ql, &qr).Add(&firstPart, &qm).Add(&firstPart, &qo).Add(&firstPart, &qk)

	// second part:
	// (1 - L_{n - 1})(z(, omegaX * alpha)()()() - z(, alpha)()()())
	// + L_{n - 1}(cw * ()()() - pw * z(, alpha)()()())
	var prodfz, prodg fr.Element
	sy1.Mul(&sy1, &etaY)
	sy2.Mul(&sy2, &etaY)
	sy3.Mul(&sy3, &etaY)
	sx1.Mul(&sx1, &etaX).Add(&sx1, &sy1).Add(&sx1, &l).Add(&sx1, &gamma)
	sx2.Mul(&sx2, &etaX).Add(&sx2, &sy2).Add(&sx2, &r).Add(&sx2, &gamma)
	sx3.Mul(&sx3, &etaX).Add(&sx3, &sy3).Add(&sx3, &o).Add(&sx3, &gamma)
	prodg.Mul(&sx1, &sx2).Mul(&prodg, &sx3)

	var alphaEta, ualphaEta, uualphaEta fr.Element
	alphaEta.Mul(&alpha, &etaX)
	ualphaEta.Mul(&alphaEta, &vk.CosetShift)
	uualphaEta.Mul(&ualphaEta, &vk.CosetShift)

	var betaEta fr.Element
	betaEta.Mul(&beta, &etaY)
	var tmp fr.Element
	prodfz.Add(&alphaEta, &betaEta).Add(&prodfz, &l).Add(&prodfz, &gamma)
	tmp.Add(&ualphaEta, &betaEta).Add(&tmp, &r).Add(&tmp, &gamma)
	prodfz.Mul(&prodfz, &tmp)
	tmp.Add(&uualphaEta, &betaEta).Add(&tmp, &o).Add(&tmp, &gamma)
	prodfz.Mul(&prodfz, &tmp).Mul(&prodfz, &z)

	var one, den fr.Element
	one.SetOne()

	var secondPart, case1, case2, Lxl, oneMinusLxL fr.Element
	Lxl.Exp(alpha, big.NewInt(int64(vk.SizeX))).Sub(&Lxl, &one)
	den.Sub(&alpha, &vk.GeneratorXInv).Inverse(&den)
	Lxl.Mul(&Lxl, &den).Mul(&Lxl, &vk.SizeXInv).Mul(&Lxl, &vk.GeneratorXInv)
	oneMinusLxL.Sub(&one, &Lxl)
	case1.Mul(&prodg, &zs).Sub(&case1, &prodfz).Mul(&case1, &oneMinusLxL)
	prodfz.Mul(&prodfz, &w)
	case2.Mul(&prodg, &ws).Sub(&case2, &prodfz).Mul(&case2, &Lxl)
	secondPart.Add(&case1, &case2)

	// third part Lx0(alpha)*(Z(beta, alpha) - 1)
	var thirdPart fr.Element
	z.Sub(&z, &one)
	thirdPart.Exp(alpha, big.NewInt(int64(vk.SizeX))).Sub(&thirdPart, &one)
	den.Sub(&alpha, &one).Inverse(&den)
	thirdPart.Mul(&thirdPart, &den).Mul(&thirdPart, &vk.SizeXInv).Mul(&thirdPart, &z)

	// forth part Ly0(beta)*(W(beta) - 1)
	var forthPart fr.Element
	w.Sub(&w, &one)
	forthPart.Exp(beta, big.NewInt(int64(vk.SizeY))).Sub(&forthPart, &one)
	den.Sub(&beta, &one).Inverse(&den)
	forthPart.Mul(&forthPart, &den).Mul(&forthPart, &vk.SizeYInv).Mul(&forthPart, &w)

	// Put it all together
	var result fr.Element
	result.Mul(&forthPart, &lambda).Add(&result, &thirdPart).Mul(&result, &lambda).Add(&result, &secondPart).Mul(&result, &lambda).Add(&result, &firstPart)

	var vanishingX fr.Element
	vanishingX.Exp(alpha, big.NewInt(int64(vk.SizeX)))
	vanishingX.Sub(&vanishingX, &one)

	var vHx fr.Element
	vHx.Mul(&hx, &vanishingX)
	result.Sub(&result, &vHx)

	var vanishingY fr.Element
	vanishingY.Exp(beta, big.NewInt(int64(vk.SizeY)))
	vanishingY.Sub(&vanishingY, &one)

	var vHy fr.Element
	vHy.Mul(&hy, &vanishingY)
	result.Sub(&result, &vHy)

	// if result != 0 return error
	if !result.IsZero() {
		return fmt.Errorf("constraints on Y are not satisfied: got %s, want 0", result.String())
	}
	return nil
}
