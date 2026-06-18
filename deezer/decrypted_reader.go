// Package deezer fournit un io.Reader qui déchiffre à la volée un flux de piste
// Deezer chiffré en Blowfish-CBC.
//
// Schéma (public, documenté) : le flux est découpé en chunks de 2048 octets.
// Un chunk sur trois (index % 3 == 0) est chiffré en Blowfish-CBC, à condition
// d'être complet (2048 octets). La clé Blowfish est dérivée du MD5 de l'ID de
// piste combiné à une constante publique.
package deezer

import (
	"crypto/cipher"
	"crypto/md5"
	"encoding/hex"
	"io"

	"golang.org/x/crypto/blowfish"
)

// bfSecret est la constante Blowfish publique de Deezer (pas un secret utilisateur).
const bfSecret = "g4el58wc0zvf9na1"

// chunkSize est la taille d'un chunk lu sur le flux source.
const chunkSize = 2048

// iv est le vecteur d'initialisation fixe utilisé pour le mode CBC.
var iv = []byte{0, 1, 2, 3, 4, 5, 6, 7}

// DecryptedReader enveloppe un flux source et déchiffre les chunks Blowfish-CBC
// au fil de la lecture. Il implémente io.Reader (et io.Closer si la source l'est).
type DecryptedReader struct {
	source       io.Reader
	bfKey        []byte
	readBuffer   []byte
	currentChunk []byte
	chunkOffset  int
	chunkIndex   int
}

// NewDecryptedReader crée un DecryptedReader pour la source et l'ID de piste donnés.
func NewDecryptedReader(source io.Reader, trackID string) *DecryptedReader {
	return &DecryptedReader{
		source:     source,
		bfKey:      getBlowfishKey(trackID),
		readBuffer: make([]byte, chunkSize),
	}
}

// Read implémente io.Reader. Il remplit p avec des données déchiffrées,
// en lisant et déchiffrant les chunks de la source au besoin.
func (r *DecryptedReader) Read(p []byte) (int, error) {
	// Lit (et déchiffre éventuellement) un nouveau chunk si le courant est épuisé.
	if r.chunkOffset >= len(r.currentChunk) {
		// ReadFull comble tout le buffer, équivalent du ReadAtLeastAsync en C#.
		n, err := io.ReadFull(r.source, r.readBuffer)
		switch err {
		case nil, io.ErrUnexpectedEOF:
			// n octets lus : buffer plein (nil) ou dernier chunk partiel (ErrUnexpectedEOF).
		case io.EOF:
			// Aucun octet restant : fin du flux.
			return 0, io.EOF
		default:
			return 0, err
		}

		chunk := make([]byte, n)
		copy(chunk, r.readBuffer[:n])

		// Un chunk sur trois est chiffré, uniquement s'il est complet.
		if r.chunkIndex%3 == 0 && n == chunkSize {
			r.decryptBlowfishCbc(chunk)
		}

		r.currentChunk = chunk
		r.chunkOffset = 0
		r.chunkIndex++
	}

	// Copie une partie du chunk courant dans la destination.
	n := copy(p, r.currentChunk[r.chunkOffset:])
	r.chunkOffset += n
	return n, nil
}

// Close ferme le flux source s'il implémente io.Closer.
func (r *DecryptedReader) Close() error {
	if c, ok := r.source.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// getBlowfishKey dérive la clé Blowfish de 16 octets à partir de l'ID de piste.
func getBlowfishKey(trackID string) []byte {
	hash := md5.Sum([]byte(trackID))
	hashHex := hex.EncodeToString(hash[:]) // 32 caractères hex minuscules

	bfKey := make([]byte, 16)
	for i := 0; i < 16; i++ {
		bfKey[i] = hashHex[i] ^ hashHex[i+16] ^ bfSecret[i]
	}
	return bfKey
}

// decryptBlowfishCbc déchiffre data sur place en Blowfish-CBC.
// data doit être un multiple de la taille de bloc (8 octets) ; 2048 l'est.
func (r *DecryptedReader) decryptBlowfishCbc(data []byte) {
	block, err := blowfish.NewCipher(r.bfKey)
	if err != nil {
		// La clé fait toujours 16 octets (Blowfish accepte 1 à 56), donc improbable.
		return
	}

	// IV recopié car CBCDecrypter mute son IV interne ; on garde un état neuf par chunk.
	ivCopy := make([]byte, len(iv))
	copy(ivCopy, iv)

	cipher.NewCBCDecrypter(block, ivCopy).CryptBlocks(data, data)
}
