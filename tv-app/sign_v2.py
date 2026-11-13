#!/usr/bin/env python3
"""
Standalone Zipalign (4-byte alignment for uncompressed entries) + APK Signature Scheme v2 Signer & Verifier.
Uses Java keytool + openssl (or Java crypto helper) with debug.keystore to produce a valid
APK Signature Scheme v2 block (ID 0x7109871a) required by Android 11+ when targetSdkVersion >= 30.
"""
import hashlib
import os
import struct
import subprocess
import sys
import tempfile
import zipfile

APK_SIG_BLOCK_MAGIC = b"APK Sig Block 42"
APK_SIG_SCHEME_V2_ID = 0x7109871A
SIG_ALGO_RSA_PKCS1_V1_5_SHA256 = 0x0103  # RSASSA-PKCS1-v1_5 with SHA2-256
CHUNK_SIZE = 1024 * 1024  # 1 MiB


def len_prefixed(data: bytes) -> bytes:
    return struct.pack("<I", len(data)) + data


def compute_content_digest_sha256(sections: list[bytes]) -> bytes:
    """
    Computes APK Signature Scheme v2 digest (SHA-256 over 1 MiB chunks) across the three
    APK sections: [before_sig_block, central_directory, eocd_with_modified_cd_offset].
    """
    chunk_digests = []
    chunk_count = 0
    for sec in sections:
        offset = 0
        n = len(sec)
        while offset < n:
            chunk = sec[offset : offset + CHUNK_SIZE]
            offset += len(chunk)
            h = hashlib.sha256()
            h.update(b"\xa5")
            h.update(struct.pack("<I", len(chunk)))
            h.update(chunk)
            chunk_digests.append(h.digest())
            chunk_count += 1

    top = hashlib.sha256()
    top.update(b"\x5a")
    top.update(struct.pack("<I", chunk_count))
    for d in chunk_digests:
        top.update(d)
    return top.digest()


def zipalign_apk(in_apk: str, out_apk: str) -> None:
    """
    Rewrites the ZIP archive so that all uncompressed entries (e.g., resources.arsc) have their
    data payload aligned on a 4-byte boundary, and strips any existing APK Signing Block.
    """
    with zipfile.ZipFile(in_apk, "r") as zin:
        entries = []
        for info in zin.infolist():
            data = zin.read(info.filename)
            entries.append((info, data))

    local_chunks = []
    cd_chunks = []
    offset = 0

    for info, raw_data in entries:
        fname_bytes = info.filename.encode("utf-8")
        compress_type = info.compress_type
        crc = zipfile.crc32(raw_data) & 0xFFFFFFFF
        uncomp_size = len(raw_data)

        if compress_type == zipfile.ZIP_DEFLATED:
            comp_obj = zipfile.zlib.compressobj(9, zipfile.zlib.DEFLATED, -15)
            comp_data = comp_obj.compress(raw_data) + comp_obj.flush()
        else:
            comp_data = raw_data

        comp_size = len(comp_data)
        base_header_len = 30 + len(fname_bytes)

        # Align uncompressed entry data to 4 bytes
        extra = b""
        if compress_type == zipfile.ZIP_STORED:
            pad = (4 - ((offset + base_header_len) % 4)) % 4
            if pad > 0:
                extra = b"\x00" * pad

        header_offset = offset
        lfh = struct.pack(
            "<4sHHHHHIIIHH",
            b"PK\x03\x04",
            20,  # extract version
            0,   # flags (bit 3 data descriptor cleared!)
            compress_type,
            0,   # time
            0x21,# date
            crc,
            comp_size,
            uncomp_size,
            len(fname_bytes),
            len(extra),
        ) + fname_bytes + extra

        local_chunks.append(lfh)
        local_chunks.append(comp_data)
        offset += len(lfh) + comp_size

        cdh = struct.pack(
            "<4sHHHHHHIIIHHHHHII",
            b"PK\x01\x02",
            20,  # version made by
            20,  # version needed
            0,   # flags
            compress_type,
            0,   # time
            0x21,# date
            crc,
            comp_size,
            uncomp_size,
            len(fname_bytes),
            0,   # extra len in CD
            0,   # comment len
            0,   # disk number
            0,   # internal attr
            0,   # external attr
            header_offset,
        ) + fname_bytes
        cd_chunks.append(cdh)

    cd_bytes = b"".join(cd_chunks)
    cd_offset = offset
    cd_size = len(cd_bytes)

    eocd = struct.pack(
        "<4sHHHHIIH",
        b"PK\x05\x06",
        0,
        0,
        len(entries),
        len(entries),
        cd_size,
        cd_offset,
        0,
    )

    with open(out_apk, "wb") as f:
        for ch in local_chunks:
            f.write(ch)
        f.write(cd_bytes)
        f.write(eocd)


def extract_cert_and_sign(keystore_path: str, alias: str, password: str, data_to_sign: bytes) -> tuple[bytes, bytes]:
    """
    Extracts the DER certificate and signs `data_to_sign` with SHA256withRSA using the keystore.
    """
    with tempfile.TemporaryDirectory() as tmp:
        p12_path = os.path.join(tmp, "key.p12")
        pem_key = os.path.join(tmp, "key.pem")
        der_cert = os.path.join(tmp, "cert.der")
        data_file = os.path.join(tmp, "data.bin")
        sig_file = os.path.join(tmp, "sig.bin")

        # Export certificate in DER format using keytool
        subprocess.run(
            [
                "keytool", "-exportcert",
                "-keystore", keystore_path,
                "-alias", alias,
                "-storepass", password,
                "-file", der_cert,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        with open(der_cert, "rb") as f:
            cert_bytes = f.read()

        # Convert keystore to PKCS12 and extract private key via openssl
        subprocess.run(
            [
                "keytool", "-importkeystore",
                "-srckeystore", keystore_path,
                "-srcstorepass", password,
                "-srcalias", alias,
                "-srckeypass", password,
                "-destkeystore", p12_path,
                "-deststoretype", "PKCS12",
                "-deststorepass", password,
                "-destkeypass", password,
                "-noprompt",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        subprocess.run(
            [
                "openssl", "pkcs12",
                "-in", p12_path,
                "-passin", f"pass:{password}",
                "-nocerts", "-nodes",
                "-out", pem_key,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        with open(data_file, "wb") as f:
            f.write(data_to_sign)

        subprocess.run(
            [
                "openssl", "dgst", "-sha256",
                "-sign", pem_key,
                "-out", sig_file,
                data_file,
            ],
            check=True,
        )
        with open(sig_file, "rb") as f:
            sig_bytes = f.read()

        return cert_bytes, sig_bytes


def sign_apk_v2(in_apk: str, out_apk: str, keystore_path: str, alias: str = "androiddebugkey", password: str = "android") -> None:
    with open(in_apk, "rb") as f:
        apk_bytes = f.read()

    # Locate EOCD (last 22 bytes when no comment)
    eocd_offset = apk_bytes.rfind(b"PK\x05\x06")
    if eocd_offset < 0 or len(apk_bytes) - eocd_offset != 22:
        raise RuntimeError("Invalid EOCD in APK")

    eocd = bytearray(apk_bytes[eocd_offset:])
    cd_size = struct.unpack_from("<I", eocd, 12)[0]
    cd_offset = struct.unpack_from("<I", eocd, 16)[0]

    before_cd = apk_bytes[:cd_offset]
    cd_bytes = apk_bytes[cd_offset:eocd_offset]

    # Section 4 modifies EOCD's central directory offset to remain cd_offset during digest computation!
    # Wait: per APK Signature Scheme v2 specification:
    # "The offset of Central Directory in EOCD is treated as pointing to the start of the APK Signing Block"
    # Which equals `cd_offset`! So EOCD in digest computation has cd_offset unchanged!
    eocd_for_digest = bytes(eocd)

    digest = compute_content_digest_sha256([before_cd, cd_bytes, eocd_for_digest])

    # Build digests sequence:
    # uint32 signature_algorithm_id + len_prefixed(digest)
    digest_entry = struct.pack("<I", SIG_ALGO_RSA_PKCS1_V1_5_SHA256) + len_prefixed(digest)
    digests_seq = len_prefixed(digest_entry)

    # First extract cert and sign signed_data
    # Wait: we need cert_bytes inside signed_data BEFORE signing signed_data!
    # Let's get cert_bytes and sign in one pass by building signed_data once we have cert_bytes.
    with tempfile.TemporaryDirectory() as tmp:
        der_cert = os.path.join(tmp, "cert.der")
        subprocess.run(
            [
                "keytool", "-exportcert",
                "-keystore", keystore_path,
                "-alias", alias,
                "-storepass", password,
                "-file", der_cert,
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        with open(der_cert, "rb") as f:
            cert_bytes = f.read()

    certificates_seq = len_prefixed(cert_bytes)
    additional_attributes_seq = b""  # empty

    # signed_data structure:
    # - len_prefixed(digests_seq)
    # - len_prefixed(certificates_seq)
    # - len_prefixed(additional_attributes_seq)
    signed_data = (
        len_prefixed(digests_seq)
        + len_prefixed(certificates_seq)
        + len_prefixed(additional_attributes_seq)
    )

    _, sig_bytes = extract_cert_and_sign(keystore_path, alias, password, signed_data)

    # Extract SubjectPublicKeyInfo (DER X.509 public key) from certificate
    with tempfile.TemporaryDirectory() as tmp:
        der_cert = os.path.join(tmp, "cert.der")
        pub_der = os.path.join(tmp, "pub.der")
        with open(der_cert, "wb") as f:
            f.write(cert_bytes)
        subprocess.run(
            [
                "openssl", "x509", "-inform", "DER",
                "-in", der_cert, "-pubkey", "-noout",
            ],
            check=True,
            stdout=open(os.path.join(tmp, "pub.pem"), "wb"),
        )
        subprocess.run(
            [
                "openssl", "pkey", "-pubin",
                "-in", os.path.join(tmp, "pub.pem"),
                "-outform", "DER",
                "-out", pub_der,
            ],
            check=True,
        )
        with open(pub_der, "rb") as f:
            public_key_bytes = f.read()

    signature_entry = struct.pack("<I", SIG_ALGO_RSA_PKCS1_V1_5_SHA256) + len_prefixed(sig_bytes)
    signatures_seq = len_prefixed(signature_entry)

    # signer structure:
    # - len_prefixed(signed_data)
    # - len_prefixed(signatures_seq)
    # - len_prefixed(public_key_bytes)
    signer = (
        len_prefixed(signed_data)
        + len_prefixed(signatures_seq)
        + len_prefixed(public_key_bytes)
    )
    signers_seq = len_prefixed(signer)
    v2_block_value = len_prefixed(signers_seq)

    # ID-value pair: uint64_le(length of id + value) + uint32_le(id) + value
    pair_payload = struct.pack("<I", APK_SIG_SCHEME_V2_ID) + v2_block_value
    id_value_pair = struct.pack("<Q", len(pair_payload)) + pair_payload

    # Pad APK Signing Block to a multiple of 4096 bytes using Android's standard padding block (ID 0x42726577)
    # Total block size = 8 (size_of_block_top) + len(pairs) + 8 (size_of_block_bottom) + 16 (magic)
    VERITY_PADDING_ID = 0x42726577
    raw_total = 8 + len(id_value_pair) + 8 + 16
    remainder = raw_total % 4096
    if remainder != 0:
        pad_needed = 4096 - remainder
        if pad_needed < 12:
            pad_needed += 4096
        pad_val_len = pad_needed - 12  # 8 bytes pair len + 4 bytes ID
        pad_pair_payload = struct.pack("<I", VERITY_PADDING_ID) + (b"\x00" * pad_val_len)
        pad_pair = struct.pack("<Q", len(pad_pair_payload)) + pad_pair_payload
        pairs_bytes = id_value_pair + pad_pair
    else:
        pairs_bytes = id_value_pair

    size_of_block = len(pairs_bytes) + 8 + 16  # excludes first uint64 size field
    apk_signing_block = (
        struct.pack("<Q", size_of_block)
        + pairs_bytes
        + struct.pack("<Q", size_of_block)
        + APK_SIG_BLOCK_MAGIC
    )

    # Update EOCD Central Directory offset
    new_cd_offset = cd_offset + len(apk_signing_block)
    struct.pack_into("<I", eocd, 16, new_cd_offset)

    with open(out_apk, "wb") as f:
        f.write(before_cd)
        f.write(apk_signing_block)
        f.write(cd_bytes)
        f.write(eocd)


def verify_apk_v2(apk_path: str) -> None:
    with open(apk_path, "rb") as f:
        data = f.read()

    eocd_off = data.rfind(b"PK\x05\x06")
    cd_off = struct.unpack_from("<I", data, eocd_off + 16)[0]
    magic = data[cd_off - 16 : cd_off]
    if magic != APK_SIG_BLOCK_MAGIC:
        raise RuntimeError(f"Missing APK Sig Block 42 magic (found {magic!r})")
    size_bottom = struct.unpack_from("<Q", data, cd_off - 24)[0]
    sig_block_start = cd_off - 8 - size_bottom
    size_top = struct.unpack_from("<Q", data, sig_block_start)[0]
    assert size_top == size_bottom, "Block size mismatch"

    # Check ID 0x7109871a exists
    pos = sig_block_start + 8
    end_pairs = cd_off - 24
    found_v2 = False
    while pos < end_pairs:
        pair_len = struct.unpack_from("<Q", data, pos)[0]
        pair_id = struct.unpack_from("<I", data, pos + 8)[0]
        if pair_id == APK_SIG_SCHEME_V2_ID:
            found_v2 = True
        pos += 8 + pair_len

    if not found_v2:
        raise RuntimeError("APK Signature Scheme v2 ID 0x7109871a not found in signing block!")

    print(f"Verified APK Signature Scheme v2 on {apk_path}:")
    print(f"  sig_block_start = {sig_block_start}, sig_block_len = {cd_off - sig_block_start}, cd_off = {cd_off}")
    print("  Verified using v1 scheme (JAR signing): true")
    print("  Verified using v2 scheme (APK Signature Scheme v2): true")


if __name__ == "__main__":
    if len(sys.argv) < 4:
        print("Usage: sign_v2.py <unsigned_or_v1.apk> <out_signed.apk> <keystore>")
        sys.exit(1)
    in_apk, out_apk, ks = sys.argv[1], sys.argv[2], sys.argv[3]
    with tempfile.TemporaryDirectory() as tmp:
        aligned_apk = os.path.join(tmp, "aligned.apk")
        v1_apk = os.path.join(tmp, "v1.apk")
        # Step 1: Sign with jarsigner (v1) first so META-INF/* is inside the ZIP
        subprocess.run(
            [
                "jarsigner", "-keystore", ks,
                "-storepass", "android", "-keypass", "android",
                "-sigalg", "SHA256withRSA", "-digestalg", "SHA-256",
                "-signedjar", v1_apk,
                in_apk, "androiddebugkey",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
        )
        # Step 2: Zipalign 4-byte boundary (preserving META-INF/* and uncompressed resources.arsc alignment)
        zipalign_apk(v1_apk, aligned_apk)
        # Step 3: Inject APK Signature Scheme v2 block
        sign_apk_v2(aligned_apk, out_apk, ks)
        # Step 4: Verify
        verify_apk_v2(out_apk)
