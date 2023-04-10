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

package plonk

import (
	"crypto/sha256"
	"math/big"
	"math/bits"
	"runtime"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	curve "github.com/consensys/gnark-crypto/ecc/bn254"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/kzg"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/fft"

	bn254witness "github.com/consensys/gnark/internal/backend/bn254/witness"

	"github.com/consensys/gnark/internal/backend/bn254/cs"

	"github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gnark/logger"
)

type Proof struct {

	// Commitments to the solution vectors
	LRO [3]kzg.Digest

	// Commitment to Z, the permutation polynomial
	Z kzg.Digest

	// Commitments to h1, h2, h3 such that h = h1 + Xh2 + X**2h3 is the quotient polynomial
	H [3]kzg.Digest

	// Batch opening proof of h1 + zeta*h2 + zeta**2h3, linearizedPolynomial, l, r, o, s1, s2
	BatchedProof kzg.BatchOpeningProof

	// Opening proof of Z at zeta*mu
	ZShiftedOpening kzg.OpeningProof
}

// Prove from the public data
func Prove(spr *cs.SparseR1CS, pk *ProvingKey, fullWitness bn254witness.Witness, opt backend.ProverConfig) (*Proof, error) {

	log := logger.Logger().With().Str("curve", spr.CurveID().String()).Int("nbConstraints", len(spr.Constraints)).Str("backend", "plonk").Logger()
	start := time.Now()
	// pick a hash function that will be used to derive the challenges
	hFunc := sha256.New()

	// create a transcript manager to apply Fiat Shamir
	fs := fiatshamir.NewTranscript(hFunc, "gamma", "beta", "alpha", "zeta")

	// result
	proof := &Proof{}

	// compute the constraint system solution
	var solution []fr.Element
	var err error
	if solution, err = spr.Solve(fullWitness, opt); err != nil {
		if !opt.Force {
			return nil, err
		} else {
			// we need to fill solution with random values
			var r fr.Element
			_, _ = r.SetRandom()
			for i := spr.NbPublicVariables + spr.NbSecretVariables; i < len(solution); i++ {
				solution[i] = r
				r.Double(&r)
			}
		}
	}

	// query l, r, o in Lagrange basis, not blinded
	evaluationLDomainSmall, evaluationRDomainSmall, evaluationODomainSmall := evaluateLROSmallDomain(spr, pk, solution)

	// save ll, lr, lo, and make a copy of them in canonical basis.
	// note that we allocate more capacity to reuse for blinded polynomials
	blindedLCanonical, blindedRCanonical, blindedOCanonical, err := computeBlindedLROCanonical(
		evaluationLDomainSmall,
		evaluationRDomainSmall,
		evaluationODomainSmall,
		&pk.Domain[0])
	if err != nil {
		return nil, err
	}

	// compute kzg commitments of bcl, bcr and bco
	if err := commitToLRO(blindedLCanonical, blindedRCanonical, blindedOCanonical, proof, pk.Vk.KZGSRS); err != nil {
		return nil, err
	}

	// The first challenge is derived using the public data: the commitments to the permutation,
	// the coefficients of the circuit, and the public inputs.
	// derive gamma from the Comm(blinded cl), Comm(blinded cr), Comm(blinded co)
	if err := bindPublicData(&fs, "gamma", *pk.Vk, fullWitness[:spr.NbPublicVariables]); err != nil {
		return nil, err
	}
	bgamma, err := fs.ComputeChallenge("gamma")
	if err != nil {
		return nil, err
	}
	var gamma fr.Element
	gamma.SetBytes(bgamma)

	// Fiat Shamir this
	beta, err := deriveRandomness(&fs, "beta")
	if err != nil {
		return nil, err
	}

	// compute Z, the permutation accumulator polynomial, in canonical basis
	// ll, lr, lo are NOT blinded
	var blindedZCanonical []fr.Element
	blindedZCanonical, err = computeBlindedZCanonical(
		evaluationLDomainSmall,
		evaluationRDomainSmall,
		evaluationODomainSmall,
		pk, beta, gamma)
	if err != nil {
		return nil, err
	}

	// commit to the blinded version of z
	// note that we explicitly double the number of tasks for the multi exp in kzg.Commit
	// this may add additional arithmetic operations, but with smaller tasks
	// we ensure that this commitment is well parallelized, without having a "unbalanced task" making
	// the rest of the code wait too long.
	if proof.Z, err = kzg.Commit(blindedZCanonical, pk.Vk.KZGSRS, runtime.NumCPU()*2); err != nil {
		return nil, err
	}

	alpha, err := deriveRandomness(&fs, "alpha", &proof.Z)
	if err != nil {
		return nil, err
	}

	qkCompletedCanonical := make([]fr.Element, pk.Domain[0].Cardinality)
	copy(qkCompletedCanonical, fullWitness[:spr.NbPublicVariables])
	copy(qkCompletedCanonical[spr.NbPublicVariables:], pk.LQk[spr.NbPublicVariables:])
	pk.Domain[0].FFTInverse(qkCompletedCanonical, fft.DIF)
	fft.BitReverse(qkCompletedCanonical)

	// compute h in canonical form
	h1, h2, h3 := computeQuotientCanonical(pk, blindedLCanonical, blindedRCanonical, blindedOCanonical, blindedZCanonical, qkCompletedCanonical, beta, gamma, alpha)

	// compute kzg commitments of h1, h2 and h3
	if err := commitToQuotient(h1, h2, h3, proof, pk.Vk.KZGSRS); err != nil {
		return nil, err
	}

	// derive zeta
	zeta, err := deriveRandomness(&fs, "zeta", &proof.H[0], &proof.H[1], &proof.H[2])
	if err != nil {
		return nil, err
	}

	// compute evaluations of (blinded version of) l, r, o, z at zeta
	blzeta := eval(blindedLCanonical, zeta)
	brzeta := eval(blindedRCanonical, zeta)
	bozeta := eval(blindedOCanonical, zeta)

	// open blinded Z at zeta*z
	var zetaShifted fr.Element
	zetaShifted.Mul(&zeta, &pk.Vk.Generator)
	proof.ZShiftedOpening, err = kzg.Open(
		blindedZCanonical,
		zetaShifted,
		pk.Vk.KZGSRS,
	)
	if err != nil {
		return nil, err
	}

	// blinded z evaluated at u*zeta
	bzuzeta := proof.ZShiftedOpening.ClaimedValue

	var (
		linearizedPolynomialCanonical []fr.Element
		linearizedPolynomialDigest    curve.G1Affine
		errLPoly                      error
	)
	linearizedPolynomialCanonical = computeLinearizedPolynomial(
		blzeta,
		brzeta,
		bozeta,
		alpha,
		beta,
		gamma,
		zeta,
		bzuzeta,
		blindedZCanonical,
		pk,
	)

	// TODO this commitment is only necessary to derive the challenge, we should
	// be able to avoid doing it and get the challenge in another way
	linearizedPolynomialDigest, errLPoly = kzg.Commit(linearizedPolynomialCanonical, pk.Vk.KZGSRS)
	if errLPoly != nil {
		return nil, errLPoly
	}
	// foldedHDigest = Comm(h1) + ζᵐ⁺²*Comm(h2) + ζ²⁽ᵐ⁺²⁾*Comm(h3)
	var bZetaPowerm, bSize big.Int
	bSize.SetUint64(pk.Domain[0].Cardinality)
	var zetaPowerm fr.Element
	zetaPowerm.Exp(zeta, &bSize)
	zetaPowerm.ToBigIntRegular(&bZetaPowerm)
	foldedHDigest := proof.H[2]
	foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm)
	foldedHDigest.Add(&foldedHDigest, &proof.H[1])                   // ζᵐ⁺²*Comm(h3)
	foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm) // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2)
	foldedHDigest.Add(&foldedHDigest, &proof.H[0])                   // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2) + Comm(h1)

	// foldedH = h1 + ζ*h2 + ζ²*h3
	foldedH := h3
	utils.Parallelize(len(foldedH), func(start, end int) {
		for i := start; i < end; i++ {
			foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζᵐ⁺²*h3
			foldedH[i].Add(&foldedH[i], &h2[i])      // ζ^{m+2)*h3+h2
			foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζ²⁽ᵐ⁺²⁾*h3+h2*ζᵐ⁺²
			foldedH[i].Add(&foldedH[i], &h1[i])      // ζ^{2(m+2)*h3+ζᵐ⁺²*h2 + h1
		}
	})

	// Batch open the first list of polynomials
	proof.BatchedProof, err = kzg.BatchOpenSinglePoint(
		[][]fr.Element{
			foldedH,
			linearizedPolynomialCanonical,
			blindedLCanonical,
			blindedRCanonical,
			blindedOCanonical,
			pk.S1Canonical,
			pk.S2Canonical,
		},
		[]kzg.Digest{
			foldedHDigest,
			linearizedPolynomialDigest,
			proof.LRO[0],
			proof.LRO[1],
			proof.LRO[2],
			pk.Vk.S[0],
			pk.Vk.S[1],
		},
		zeta,
		hFunc,
		pk.Vk.KZGSRS,
	)

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")

	if err != nil {
		return nil, err
	}

	return proof, nil

}

// eval evaluates c at p
func eval(c []fr.Element, p fr.Element) fr.Element {
	var r fr.Element
	for i := len(c) - 1; i >= 0; i-- {
		r.Mul(&r, &p).Add(&r, &c[i])
	}
	return r
}

// fills proof.LRO with kzg commits of bcl, bcr and bco
func commitToLRO(bcl, bcr, bco []fr.Element, proof *Proof, srs *kzg.SRS) error {
	n := runtime.NumCPU() / 2
	var err0, err1, err2 error
	proof.LRO[0], err0 = kzg.Commit(bcl, srs, n)
	if err0 != nil {
		return err0
	}
	proof.LRO[1], err1 = kzg.Commit(bcr, srs, n)
	if err1 != nil {
		return err1
	}
	if proof.LRO[2], err2 = kzg.Commit(bco, srs, n); err2 != nil {
		return err2
	}
	return nil
}

func commitToQuotient(h1, h2, h3 []fr.Element, proof *Proof, srs *kzg.SRS) error {
	n := runtime.NumCPU() / 2
	var err0, err1, err2 error
	proof.H[0], err0 = kzg.Commit(h1, srs, n)
	if err0 != nil {
		return err0
	}
	proof.H[1], err1 = kzg.Commit(h2, srs, n)
	if err1 != nil {
		return err1
	}
	if proof.H[2], err2 = kzg.Commit(h3, srs, n); err2 != nil {
		return err2
	}
	return nil
}

// computeBlindedLROCanonical l, r, o in canonical basis with blinding
func computeBlindedLROCanonical(ll, lr, lo []fr.Element, domain *fft.Domain) (cl, cr, co []fr.Element, err error) {

	// note that bcl, bcr and bco reuses cl, cr and co memory
	cl = make([]fr.Element, domain.Cardinality, domain.Cardinality+2)
	cr = make([]fr.Element, domain.Cardinality, domain.Cardinality+2)
	co = make([]fr.Element, domain.Cardinality, domain.Cardinality+2)

	copy(cl, ll)
	domain.FFTInverse(cl, fft.DIF)
	fft.BitReverse(cl)
	copy(cr, lr)
	domain.FFTInverse(cr, fft.DIF)
	fft.BitReverse(cr)
	copy(co, lo)
	domain.FFTInverse(co, fft.DIF)
	fft.BitReverse(co)
	err = nil
	return
}

// blindPoly blinds a polynomial by adding a Q(X)*(X**degree-1), where deg Q = order.
//
// * cp polynomial in canonical form
// * rou root of unity, meaning the blinding factor is multiple of X**rou-1
// * bo blinding order,  it's the degree of Q, where the blinding is Q(X)*(X**degree-1)
//
// WARNING:
// pre condition degree(cp) ⩽ rou + bo
// pre condition cap(cp) ⩾ int(totalDegree + 1)
func blindPoly(cp []fr.Element, rou, bo uint64) ([]fr.Element, error) {
	// degree of the blinded polynomial is max(rou+order, cp.Degree)
	totalDegree := rou + bo

	// re-use cp
	res := cp[:totalDegree+1]

	// random polynomial
	blindingPoly := make([]fr.Element, bo+1)
	for i := uint64(0); i < bo+1; i++ {
		if _, err := blindingPoly[i].SetRandom(); err != nil {
			return nil, err
		}
	}

	// blinding
	for i := uint64(0); i < bo+1; i++ {
		res[i].Sub(&res[i], &blindingPoly[i])
		res[rou+i].Add(&res[rou+i], &blindingPoly[i])
	}

	return res, nil

}

// evaluateLROSmallDomain extracts the solution l, r, o, and returns it in lagrange form.
// solution = [ public | secret | internal ]
func evaluateLROSmallDomain(spr *cs.SparseR1CS, pk *ProvingKey, solution []fr.Element) ([]fr.Element, []fr.Element, []fr.Element) {

	s := int(pk.Domain[0].Cardinality)

	var l, r, o []fr.Element
	l = make([]fr.Element, s)
	r = make([]fr.Element, s)
	o = make([]fr.Element, s)
	s0 := solution[0]

	for i := 0; i < spr.NbPublicVariables; i++ { // placeholders
		l[i] = solution[i]
		r[i] = s0
		o[i] = s0
	}
	offset := spr.NbPublicVariables
	for i := 0; i < len(spr.Constraints); i++ { // constraints
		l[offset+i] = solution[spr.Constraints[i].L.WireID()]
		r[offset+i] = solution[spr.Constraints[i].R.WireID()]
		o[offset+i] = solution[spr.Constraints[i].O.WireID()]
	}
	offset += len(spr.Constraints)

	for i := 0; i < s-offset; i++ { // offset to reach 2**n constraints (where the id of l,r,o is 0, so we assign solution[0])
		l[offset+i] = s0
		r[offset+i] = s0
		o[offset+i] = s0
	}

	return l, r, o

}

// computeZ computes Z, in canonical basis, where:
//
// * Z of degree n (domainNum.Cardinality)
// * Z(1)=1
// 								   (l(g^k)+β*g^k+γ)*(r(g^k)+uβ*g^k+γ)*(o(g^k)+u²β*g^k+γ)
// * for i>0: Z(gⁱ) = Π_{k<i} -------------------------------------------------------
//								     (l(g^k)+β*s1(g^k)+γ)*(r(g^k)+β*s2(g^k)+γ)*(o(g^k)+β*s3(\g^k)+γ)
//
//	* l, r, o are the solution in Lagrange basis, evaluated on the small domain
func computeBlindedZCanonical(l, r, o []fr.Element, pk *ProvingKey, beta, gamma fr.Element) ([]fr.Element, error) {

	// note that z has more capacity has its memory is reused for blinded z later on
	z := make([]fr.Element, pk.Domain[0].Cardinality, pk.Domain[0].Cardinality+3)
	nbElmts := int(pk.Domain[0].Cardinality)
	gInv := make([]fr.Element, pk.Domain[0].Cardinality)

	z[0].SetOne()
	gInv[0].SetOne()

	evaluationIDSmallDomain := getIDSmallDomain(&pk.Domain[0])

	utils.Parallelize(nbElmts-1, func(start, end int) {

		var f [3]fr.Element
		var g [3]fr.Element

		for i := start; i < end; i++ {

			f[0].Mul(&evaluationIDSmallDomain[i], &beta).Add(&f[0], &l[i]).Add(&f[0], &gamma)           //lᵢ+g^i*β+γ
			f[1].Mul(&evaluationIDSmallDomain[i+nbElmts], &beta).Add(&f[1], &r[i]).Add(&f[1], &gamma)   //rᵢ+u*g^i*β+γ
			f[2].Mul(&evaluationIDSmallDomain[i+2*nbElmts], &beta).Add(&f[2], &o[i]).Add(&f[2], &gamma) //oᵢ+u²*g^i*β+γ

			g[0].Mul(&evaluationIDSmallDomain[pk.Permutation[i]], &beta).Add(&g[0], &l[i]).Add(&g[0], &gamma)           //lᵢ+s₁(g^i)*β+γ
			g[1].Mul(&evaluationIDSmallDomain[pk.Permutation[i+nbElmts]], &beta).Add(&g[1], &r[i]).Add(&g[1], &gamma)   //rᵢ+s₂(g^i)*β+γ
			g[2].Mul(&evaluationIDSmallDomain[pk.Permutation[i+2*nbElmts]], &beta).Add(&g[2], &o[i]).Add(&g[2], &gamma) //oᵢ+s₃(g^i)*β+γ

			f[0].Mul(&f[0], &f[1]).Mul(&f[0], &f[2]) // (lᵢ+g^i*β+γ)*(rᵢ+u*g^i*β+γ)*(oᵢ+u²*g^i*β+γ)
			g[0].Mul(&g[0], &g[1]).Mul(&g[0], &g[2]) //  (lᵢ+s₁(g^i)*β+γ)*(rᵢ+s₂(g^i)*β+γ)*(oᵢ+s₃(g^i)*β+γ)

			gInv[i+1] = g[0]
			z[i+1] = f[0]
		}
	})

	gInv = fr.BatchInvert(gInv)
	for i := 1; i < nbElmts; i++ {
		z[i].Mul(&z[i], &z[i-1]).
			Mul(&z[i], &gInv[i])
	}

	pk.Domain[0].FFTInverse(z, fft.DIF)
	fft.BitReverse(z)

	return z, nil

}

// evaluateXnMinusOneDomainBigCoset evalutes Xᵐ-1 on DomainBig coset
func evaluateXnMinusOneDomainBigCoset(domainBig, domainSmall *fft.Domain) []fr.Element {

	ratio := domainBig.Cardinality / domainSmall.Cardinality

	res := make([]fr.Element, ratio)

	expo := big.NewInt(int64(domainSmall.Cardinality))
	res[0].Exp(domainBig.FrMultiplicativeGen, expo)

	var t fr.Element
	t.Exp(domainBig.Generator, big.NewInt(int64(domainSmall.Cardinality)))

	for i := 1; i < int(ratio); i++ {
		res[i].Mul(&res[i-1], &t)
	}

	var one fr.Element
	one.SetOne()
	for i := 0; i < int(ratio); i++ {
		res[i].Sub(&res[i], &one)
	}

	return res
}

// computeQuotientCanonical computes h in canonical form, split as h1+X^mh2+X²mh3 such that
//
// ql(X)L(X)+qr(X)R(X)+qm(X)L(X)R(X)+qo(X)O(X)+k(X) + α.(z(μX)*g₁(X)*g₂(X)*g₃(X)-z(X)*f₁(X)*f₂(X)*f₃(X)) + α²*L₁(X)*(Z(X)-1)= h(X)Z(X)
//
// constraintInd, constraintOrdering are evaluated on the big domain (coset).
func computeQuotientCanonical(pk *ProvingKey, lCanonicalX, rCanonicalX, oCanonicalX, zCanonicalX, qkCompletedCanonical []fr.Element, eta, gamma, lambda fr.Element) ([]fr.Element, []fr.Element, []fr.Element) {
	ratio := pk.Domain[1].Cardinality / pk.Domain[0].Cardinality

	// Compute the power of domain[1].Generator with bit-reversed order.
	factorsBR := make([]fr.Element, ratio)
	factorsBR[0].SetOne()
	for i := 1; i < int(ratio); i++ {
		factorsBR[i].Mul(&factorsBR[i-1], &pk.Domain[1].Generator)
	}
	fft.BitReverse(factorsBR)

	// Variables needed in permutation constraint.
	n := pk.Domain[0].Cardinality
	nn := uint64(64 - bits.TrailingZeros64(uint64(pk.Domain[0].Cardinality)))
	var cosetShiftEta, cosetShiftSquareEta fr.Element
	cosetShiftEta.Mul(&pk.Vk.CosetShift, &eta)
	cosetShiftSquareEta.Mul(&cosetShiftEta, &pk.Vk.CosetShift)

	var one fr.Element
	one.SetOne()
	Lag0 := make([]fr.Element, pk.Domain[0].Cardinality)
	for i := 0; i < int(pk.Domain[0].Cardinality); i++ {
		Lag0[i].Set(&pk.Domain[0].CardinalityInv)
	}

	h := make([]fr.Element, pk.Domain[1].Cardinality)
	for _j := 0; _j < int(ratio); _j++ {
		// Compute FFT part for each polynomial.
		lag0 := pk.Domain[0].FFTPart(Lag0, fft.DIF, factorsBR[_j], true)

		s1 := pk.Domain[0].FFTPart(pk.S1Canonical, fft.DIF, factorsBR[_j], true)
		s2 := pk.Domain[0].FFTPart(pk.S2Canonical, fft.DIF, factorsBR[_j], true)
		s3 := pk.Domain[0].FFTPart(pk.S3Canonical, fft.DIF, factorsBR[_j], true)
		z := pk.Domain[0].FFTPart(zCanonicalX, fft.DIF, factorsBR[_j], true)

		ql := pk.Domain[0].FFTPart(pk.Ql, fft.DIF, factorsBR[_j], true)
		qr := pk.Domain[0].FFTPart(pk.Qr, fft.DIF, factorsBR[_j], true)
		qm := pk.Domain[0].FFTPart(pk.Qm, fft.DIF, factorsBR[_j], true)
		qo := pk.Domain[0].FFTPart(pk.Qo, fft.DIF, factorsBR[_j], true)
		qk := pk.Domain[0].FFTPart(qkCompletedCanonical, fft.DIF, factorsBR[_j], true)

		l := pk.Domain[0].FFTPart(lCanonicalX, fft.DIF, factorsBR[_j], true)
		r := pk.Domain[0].FFTPart(rCanonicalX, fft.DIF, factorsBR[_j], true)
		o := pk.Domain[0].FFTPart(oCanonicalX, fft.DIF, factorsBR[_j], true)
	
		hStart := uint64(_j) * n
		utils.Parallelize(int(n), func(start, end int) {
			var f, g [3]fr.Element
			var t0, t1 fr.Element
			var ID fr.Element
			ID.Exp(pk.Domain[0].Generator, big.NewInt(int64(start))).
				Mul(&ID, &factorsBR[_j]).
				Mul(&ID, &pk.Domain[1].FrMultiplicativeGen)
			
			for i := uint64(start); i < uint64(end); i++ {
				_i := bits.Reverse64(uint64(i)) >> nn
				_is := bits.Reverse64(uint64((i + 1)) & (n - 1)) >> nn

				// Compute permutation constraints L0(X)*(z(X)-1)
				h[hStart + _i].Sub(&z[_i], &one).Mul(&h[hStart + _i], &lag0[_i])
				
				// Compute permutation constraints z(mu*X)*g1(X)*g2(X)*g3(X) - z(X)*f1(X)*f2(X)*f3(X)
				f[0].Mul(&ID, &eta).Add(&f[0], &l[_i]).Add(&f[0], &gamma)
				f[1].Mul(&ID, &cosetShiftEta).Add(&f[1], &r[_i]).Add(&f[1], &gamma)
				f[2].Mul(&ID, &cosetShiftSquareEta).Add(&f[2], &o[_i]).Add(&f[2], &gamma)

				g[0].Mul(&s1[_i], &eta).Add(&g[0], &l[_i]).Add(&g[0], &gamma)
				g[1].Mul(&s2[_i], &eta).Add(&g[1], &r[_i]).Add(&g[1], &gamma)
				g[2].Mul(&s3[_i], &eta).Add(&g[2], &o[_i]).Add(&g[2], &gamma)

				f[0].Mul(&f[0], &f[1]).Mul(&f[0], &f[2]).Mul(&f[0], &z[_i])
				g[0].Mul(&g[0], &g[1]).Mul(&g[0], &g[2]).Mul(&g[0], &z[_is])

				f[0].Sub(&g[0], &f[0])
				h[hStart + _i].Mul(&h[hStart + _i], &lambda).Add(&h[hStart + _i], &f[0])
				ID.Mul(&ID, &pk.Domain[0].Generator)

				// Compute gate constraint
				t1.Mul(&qm[_i], &r[_i])
				t1.Add(&t1, &ql[_i])
				t1.Mul(&t1, &l[_i])
	
				t0.Mul(&qr[_i], &r[_i])
				t0.Add(&t0, &t1)
	
				t1.Mul(&qo[_i], &o[_i])
				t0.Add(&t0, &t1).Add(&t0, &qk[_i])
				h[hStart + _i].Mul(&h[hStart + _i], &lambda).Add(&h[hStart + _i], &t0)
			}
		})
	}

	XnMinusOneBig := evaluateXnMinusOneDomainBigCoset(&pk.Domain[1], &pk.Domain[0])
	XnMinusOneBig = fr.BatchInvert(XnMinusOneBig)
	nn2 := uint64(64 - bits.TrailingZeros64(uint64(pk.Domain[1].Cardinality)))
	utils.Parallelize(int(pk.Domain[1].Cardinality), func(start, end int) {
		for _i := uint64(start); _i < uint64(end); _i++ {
			i := bits.Reverse64(_i) >> nn2
			h[_i].Mul(&h[_i], &XnMinusOneBig[i % ratio])
		}
	})
	pk.Domain[1].FFTInverse(h, fft.DIT, true)

	h1 := h[:n]
	h2 := h[n: 2*n]
	h3 := h[2*n: 3*n]

	return h1, h2, h3
}

// computeLinearizedPolynomial computes the linearized polynomial in canonical basis.
// The purpose is to commit and open all in one ql, qr, qm, qo, qk.
// * lZeta, rZeta, oZeta are the evaluation of l, r, o at zeta
// * z is the permutation polynomial, zu is Z(μX), the shifted version of Z
// * pk is the proving key: the linearized polynomial is a linear combination of ql, qr, qm, qo, qk.
//
// The Linearized polynomial is:
//
// α²*L₁(ζ)*Z(X)
// + α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ))
// + l(ζ)*Ql(X) + l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X)
func computeLinearizedPolynomial(lZeta, rZeta, oZeta, alpha, beta, gamma, zeta, zu fr.Element, blindedZCanonical []fr.Element, pk *ProvingKey) []fr.Element {

	// first part: individual constraints
	var rl fr.Element
	rl.Mul(&rZeta, &lZeta)

	// second part:
	// Z(μζ)(l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*β*s3(X)-Z(X)(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ)
	var s1, s2 fr.Element
	chS1 := make(chan struct{}, 1)
	go func() {
		s1 = eval(pk.S1Canonical, zeta)                      // s1(ζ)
		s1.Mul(&s1, &beta).Add(&s1, &lZeta).Add(&s1, &gamma) // (l(ζ)+β*s1(ζ)+γ)
		close(chS1)
	}()
	tmp := eval(pk.S2Canonical, zeta)                        // s2(ζ)
	tmp.Mul(&tmp, &beta).Add(&tmp, &rZeta).Add(&tmp, &gamma) // (r(ζ)+β*s2(ζ)+γ)
	<-chS1
	s1.Mul(&s1, &tmp).Mul(&s1, &zu).Mul(&s1, &beta) // (l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)

	var uzeta, uuzeta fr.Element
	uzeta.Mul(&zeta, &pk.Vk.CosetShift)
	uuzeta.Mul(&uzeta, &pk.Vk.CosetShift)

	s2.Mul(&beta, &zeta).Add(&s2, &lZeta).Add(&s2, &gamma)      // (l(ζ)+β*ζ+γ)
	tmp.Mul(&beta, &uzeta).Add(&tmp, &rZeta).Add(&tmp, &gamma)  // (r(ζ)+β*u*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)
	tmp.Mul(&beta, &uuzeta).Add(&tmp, &oZeta).Add(&tmp, &gamma) // (o(ζ)+β*u²*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	s2.Neg(&s2)                                                 // -(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

	// third part L₁(ζ)*α²*Z
	var lagrangeZeta, one, den, frNbElmt fr.Element
	one.SetOne()
	nbElmt := int64(pk.Domain[0].Cardinality)
	lagrangeZeta.Set(&zeta).
		Exp(lagrangeZeta, big.NewInt(nbElmt)).
		Sub(&lagrangeZeta, &one)
	frNbElmt.SetUint64(uint64(nbElmt))
	den.Sub(&zeta, &one).
		Inverse(&den)
	lagrangeZeta.Mul(&lagrangeZeta, &den). // L₁ = (ζⁿ⁻¹)/(ζ-1)
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &pk.Domain[0].CardinalityInv) // (1/n)*α²*L₁(ζ)

	linPol := make([]fr.Element, len(blindedZCanonical))
	copy(linPol, blindedZCanonical)

	utils.Parallelize(len(linPol), func(start, end int) {

		var t0, t1 fr.Element

		for i := start; i < end; i++ {

			linPol[i].Mul(&linPol[i], &s2) // -Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

			if i < len(pk.S3Canonical) {

				t0.Mul(&pk.S3Canonical[i], &s1) // (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*β*s3(X)

				linPol[i].Add(&linPol[i], &t0)
			}

			linPol[i].Mul(&linPol[i], &alpha) // α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ))

			if i < len(pk.Qm) {

				t1.Mul(&pk.Qm[i], &rl) // linPol = linPol + l(ζ)r(ζ)*Qm(X)
				t0.Mul(&pk.Ql[i], &lZeta)
				t0.Add(&t0, &t1)
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + l(ζ)*Ql(X)

				t0.Mul(&pk.Qr[i], &rZeta)
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + r(ζ)*Qr(X)

				t0.Mul(&pk.Qo[i], &oZeta).Add(&t0, &pk.CQk[i])
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + o(ζ)*Qo(X) + Qk(X)
			}

			t0.Mul(&blindedZCanonical[i], &lagrangeZeta)
			linPol[i].Add(&linPol[i], &t0) // finish the computation
		}
	})

	return linPol
}
