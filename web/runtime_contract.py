"""Shared compatibility contract for the portable launcher and both web apps."""
BUILD_VERSION = 'web-20261115-manager-pem-dev1'
PROTOCOL_VERSION = 1
APPLICATIONS = {'claude': 'claude-account-wizard', 'manager': 'local-sk-account-manager'}


def health(application, **fields):
    return {'application': application, 'version': BUILD_VERSION, 'protocol': PROTOCOL_VERSION, **fields}


def require_compatible(response, application):
    if not isinstance(response, dict) or response.get('application') != application:
        raise RuntimeError('本机端口属于其他程序，请选择其他端口。')
    if response.get('version') != BUILD_VERSION or response.get('protocol') != PROTOCOL_VERSION:
        raise RuntimeError('检测到旧版或版本不一致的网页助手。请先关闭原助手黑窗口，再启动新版；不会自动终止原授权。')
