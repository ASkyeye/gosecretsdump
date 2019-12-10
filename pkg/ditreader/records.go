package ditreader

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/C-Sto/gosecretsdump/pkg/esent"
	"golang.org/x/text/encoding/unicode"
)

func (d *DitReader) DecryptRecord(record esent.Esent_record) (DumpedHash, error) {
	dh := DumpedHash{}
	v, _ := record.GetBytVal(nobjectSid)
	sid, err := NewSAMRRPCSID(v) //record.Column[z].BytVal)
	if err != nil {
		return dh, err
	}
	dh.Rid = sid.FormatCanonical()[strings.LastIndex(sid.FormatCanonical(), "-")+1:]

	//lm hash
	if v, err := record.StrVal(ndBCSPwd); err != nil && len(v) > 0 {
		//if record.Column[ndBCSPwd"]].StrVal != "" {
		tmpLM := []byte{}
		b, _ := record.GetBytVal(ndBCSPwd)
		encryptedLM, err := NewCryptedHash(b)
		if err != nil {
			return dh, err
		}
		if bytes.Compare(encryptedLM.Header[:4], []byte("\x13\x00\x00\x00")) == 0 {
			encryptedLMW := NewCryptedHashW16(b)
			pekIndex := encryptedLMW.Header
			tmpLM, err = decryptAES(d.pek[pekIndex[4]], encryptedLMW.EncrypedHash[:16], encryptedLMW.KeyMaterial[:])
			if err != nil {
				return dh, err
			}
		} else {
			tmpLM, err = d.removeRC4(encryptedLM)
			if err != nil {
				return dh, err
			}
		}
		dh.LMHash, err = removeDES(tmpLM, dh.Rid)
		if err != nil {
			return dh, err
		}
	} else {
		//hard coded empty lm hash
		dh.LMHash, _ = hex.DecodeString("aad3b435b51404eeaad3b435b51404ee")
	}

	//nt hash
	if v, _ := record.GetBytVal(nunicodePwd); len(v) > 0 { //  record.Column[nunicodePwd"]].BytVal; len(v) > 0 {
		tmpNT := []byte{}
		encryptedNT, err := NewCryptedHash(v)
		if err != nil {
			return dh, err
		}
		if bytes.Compare(encryptedNT.Header[:4], []byte("\x13\x00\x00\x00")) == 0 {
			encryptedNTW := NewCryptedHashW16(v)
			pekIndex := encryptedNTW.Header
			tmpNT, err = decryptAES(d.pek[pekIndex[4]], encryptedNTW.EncrypedHash[:16], encryptedNTW.KeyMaterial[:])
			if err != nil {
				return dh, err
			}
		} else {
			tmpNT, err = d.removeRC4(encryptedNT)
			if err != nil {
				return dh, err
			}
		}
		dh.NTHash, err = removeDES(tmpNT, dh.Rid)
		if err != nil {
			return dh, err
		}
	} else {
		//hard coded empty NTLM hash
		dh.NTHash, _ = hex.DecodeString("31D6CFE0D16AE931B73C59D7E0C089C0")
	}

	//username
	if v, err := record.StrVal(nuserPrincipalName); err != nil && v != "" && strings.Contains(v, "@") { //record.Column[nuserPrincipalName"]].StrVal; v != "" {
		rec := v
		domain := rec[strings.LastIndex(rec, "@")+1:]
		dh.Username = fmt.Sprintf("%s\\%s", domain, v[:strings.LastIndex(rec, "@")])
	} else {
		v, _ := record.StrVal(nsAMAccountName)
		dh.Username = fmt.Sprintf("%s", v)
	}

	if v, _ := record.GetLongVal(nuserAccountControl); v != 0 { // record.Column[nuserAccountControl"]].Long; v != 0 {
		dh.UAC = decodeUAC(int(v))
	}

	if val, _ := record.GetBytVal(nsupplementalCredentials); len(val) > 24 {
		//if val := record.Column[nsupplementalCredentials"]]; len(val.BytVal) > 24 {
		var err error
		dh.Supp, err = d.decryptSupp(record)
		if err != nil {
			fmt.Println("Error: ", err)
		}
	}

	return dh, nil
}

func (d DitReader) decryptSupp(record esent.Esent_record) (SuppInfo, error) {
	r := SuppInfo{}
	bval, _ := record.GetBytVal(nsupplementalCredentials) // record.Column[nsupplementalCredentials"]]
	if len(bval) > 24 {                                   //is the value above the minimum for plaintex passwords?
		username, _ := record.StrVal(nsAMAccountName)
		var plainBytes []byte
		//check if the record is something something? has a UPN?
		if v, _ := record.StrVal(nuserPrincipalName); v != "" { //record.Column[nuserPrincipalName"]].StrVal != "" {
			domain := v
			parts := strings.Split(domain, "@")
			domain = parts[len(parts)-1]
			username = fmt.Sprintf("%s\\%s", domain, username)
		}
		//fmt.Println(val.BytVal)
		ct, err := NewCryptedHash(bval)
		if err != nil {
			return r, err
		}
		//ct := crypted_hash{}.Init(val.BytVal)

		//check for windows 2016 tp4
		if bytes.Compare(ct.Header[:4], []byte{0x13, 0, 0, 0}) == 0 {
			//fmt.Println("TODO: WINDOWS 2016 SUPP DATA FOR PLAINTEXT")
			pekIndex := binary.LittleEndian.Uint16(ct.Header[4:6])
			plainBytes, err = decryptAES(d.pek[pekIndex],
				ct.EncryptedHash[4:],
				ct.KeyMaterial[:])
			if err != nil {
				return r, err
			}
		} else {
			plainBytes, err = d.removeRC4(ct)
			if err != nil {
				return r, err
			}
		}
		if len(plainBytes) < 100 {
			return r, fmt.Errorf("Bad length for user properties: expecting >100 got %d ", len(plainBytes))
		}
		props := NewSAMRUserProperties(plainBytes)

		for _, x := range props.Properties {
			//apparently we should care about kerberos-newer-keys, but I don't really want to at the moment
			s, e := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().String(string(x.PropertyName))
			if e != nil {
				continue
			}
			if strings.Compare(s, "Primary:CLEARTEXT") == 0 { //awwww yis
				//try decode the thing first
				nhex, err := hex.DecodeString(string(x.PropertyValue))
				if err != nil {
					continue
				}
				sdec, err := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().String(string(nhex))
				if err != nil {
					//check for machien key thingo here I guess
					continue
				}
				if !isASCII(sdec) {
					sdec = string(x.PropertyValue)
					r.NotASCII = true
				}
				r.Username = username
				r.ClearPassword = sdec
			}

		}
	}

	return r, nil
}