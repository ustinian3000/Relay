"""In-memory SSH private-key parsing shared by the two local web apps."""
import io
import paramiko
from cryptography.hazmat.primitives import serialization
from cryptography.exceptions import UnsupportedAlgorithm


class PrivateKeyError(Exception):
    pass


def load_private_key(content):
    try:
        encoded = content.encode('utf-8')
        if 'BEGIN OPENSSH PRIVATE KEY' in content:
            key = serialization.load_ssh_private_key(encoded, password=None)
        else:
            key = serialization.load_pem_private_key(encoded, password=None)
        normalized = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.OpenSSH,
                                       serialization.NoEncryption()).decode()
    except TypeError:
        raise PrivateKeyError('该私钥已加密，请先加入 SSH Agent 后使用 Agent 登录，或选择未加密的私钥文件。') from None
    except (ValueError, UnsupportedAlgorithm):
        raise PrivateKeyError('无法读取私钥文件，请选择有效的 PEM 或 OpenSSH 私钥，不要选择公钥或证书。') from None
    for key_type in (paramiko.RSAKey, paramiko.ECDSAKey, paramiko.Ed25519Key):
        try:
            return key_type.from_private_key(io.StringIO(normalized))
        except (paramiko.SSHException, ValueError):
            continue
    raise PrivateKeyError('该私钥类型不受支持，请使用 RSA、ECDSA 或 Ed25519 私钥。')

