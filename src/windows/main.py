import os
import sys
import subprocess
import threading
import time
import socket
import json
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs
import win32serviceutil
import win32service
import win32event
import servicemanager

SERVICE_NAME = "RocketMan_Tun_Service"
SERVICE_DISPLAY_NAME = "RocketMan Tunnel Service"
HTTP_PORT = 5020
APP_PING_URL = "http://localhost:8081/ping"
APP_CHECK_INTERVAL = 2  # секунды


class TunnelManager:
    """Управление процессом sing-box"""
    
    def __init__(self):
        self.process = None
        self.lock = threading.Lock()
        self.singbox_path = None
        self.config_path = None
    
    def start(self, username, appname):
        """Запускает туннель с указанными параметрами"""
        with self.lock:
            if self.is_running():
                return {"status": "already_running", "pid": self.process.pid}
            
            try:
                # Формируем пути на основе параметров
                base_path = os.path.join("C:\\Users", username, "AppData", "Roaming", appname, ".sing-box")
                self.singbox_path = os.path.join(base_path, "sing-box.exe")
                self.config_path = os.path.join(base_path, "sing-box-auto.json")
                
                if not os.path.exists(self.singbox_path):
                    return {"status": "error", "message": f"sing-box not found: {self.singbox_path}"}
                
                if not os.path.exists(self.config_path):
                    return {"status": "error", "message": f"Config not found: {self.config_path}"}
                
                self.process = subprocess.Popen(
                    [self.singbox_path, "run", "-c", self.config_path],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    creationflags=subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.CREATE_NO_WINDOW
                )
                
                # Даем процессу время на запуск
                time.sleep(0.5)
                
                if self.process.poll() is not None:
                    stderr = self.process.stderr.read().decode('utf-8', errors='ignore') if self.process.stderr else ""
                    return {
                        "status": "error", 
                        "message": f"Process exited immediately. Error: {stderr[:200]}"
                    }
                
                return {
                    "status": "started", 
                    "pid": self.process.pid,
                    "singbox_path": self.singbox_path,
                    "config_path": self.config_path
                }
            
            except Exception as e:
                return {"status": "error", "message": str(e)}
    
    def stop(self):
        """Останавливает туннель"""
        with self.lock:
            if not self.is_running():
                return {"status": "not_running"}
            
            try:
                self.process.terminate()
                
                # Ждем завершения с таймаутом
                for _ in range(50):  # 5 секунд
                    if self.process.poll() is not None:
                        break
                    time.sleep(0.1)
                
                # Если не завершился, убиваем принудительно
                if self.process.poll() is None:
                    self.process.kill()
                    self.process.wait()
                
                self.process = None
                return {"status": "stopped"}
            
            except Exception as e:
                self.process = None
                return {"status": "error", "message": str(e)}
    
    def is_running(self):
        """Проверяет, запущен ли туннель"""
        return self.process is not None and self.process.poll() is None
    
    def get_status(self):
        """Возвращает статус туннеля"""
        if self.is_running():
            return {
                "status": "running", 
                "pid": self.process.pid,
                "singbox_path": self.singbox_path,
                "config_path": self.config_path
            }
        return {"status": "stopped"}


class AppMonitor:
    """Мониторинг основного приложения"""
    
    def __init__(self, tunnel_manager, ping_url, check_interval):
        self.tunnel_manager = tunnel_manager
        self.ping_url = ping_url
        self.check_interval = check_interval
        self.is_running = False
        self.thread = None
        self.consecutive_failures = 0
        self.max_failures = 3  # Количество неудачных попыток перед остановкой
    
    def start(self):
        """Запускает мониторинг"""
        if self.is_running:
            return
        
        self.is_running = True
        self.consecutive_failures = 0
        self.thread = threading.Thread(
            target=self._monitor_loop,
            daemon=True,
            name="AppMonitorThread"
        )
        self.thread.start()
        servicemanager.LogInfoMsg(f"{SERVICE_NAME}: App monitor started")
    
    def stop(self):
        """Останавливает мониторинг"""
        self.is_running = False
        if self.thread:
            self.thread.join(timeout=5)
        servicemanager.LogInfoMsg(f"{SERVICE_NAME}: App monitor stopped")
    
    def _check_app_alive(self):
        """Проверяет, отвечает ли основное приложение"""
        try:
            req = urllib.request.Request(
                self.ping_url,
                method='GET',
                headers={'User-Agent': 'RocketMan-Service'}
            )
            
            with urllib.request.urlopen(req, timeout=2) as response:
                data = response.read().decode('utf-8')
                # Ожидаем "pong" или JSON с {"status": "pong"}
                return data.strip().lower() == 'pong' or 'pong' in data.lower()
        
        except (urllib.error.URLError, urllib.error.HTTPError, socket.timeout, Exception):
            return False
    
    def _monitor_loop(self):
        """Основной цикл мониторинга"""
        while self.is_running:
            try:
                # Проверяем только если туннель запущен
                if self.tunnel_manager.is_running():
                    app_alive = self._check_app_alive()
                    
                    if app_alive:
                        # Приложение отвечает - сбрасываем счётчик
                        if self.consecutive_failures > 0:
                            servicemanager.LogInfoMsg(
                                f"{SERVICE_NAME}: Main app reconnected"
                            )
                        self.consecutive_failures = 0
                    else:
                        # Приложение не отвечает
                        self.consecutive_failures += 1
                        
                        if self.consecutive_failures >= self.max_failures:
                            servicemanager.LogWarningMsg(
                                f"{SERVICE_NAME}: Main app not responding "
                                f"({self.consecutive_failures} checks), stopping tunnel"
                            )
                            
                            # Останавливаем туннель
                            result = self.tunnel_manager.stop()
                            servicemanager.LogInfoMsg(
                                f"{SERVICE_NAME}: Tunnel stopped due to app disconnection. "
                                f"Result: {result}"
                            )
                            
                            # Сбрасываем счётчик
                            self.consecutive_failures = 0
                
                # Ждём перед следующей проверкой
                time.sleep(self.check_interval)
            
            except Exception as e:
                servicemanager.LogErrorMsg(
                    f"{SERVICE_NAME}: Monitor error: {e}"
                )
                time.sleep(self.check_interval)


class ControlHTTPHandler(BaseHTTPRequestHandler):
    """HTTP обработчик для команд управления"""
    
    def log_message(self, format, *args):
        """Отключаем стандартные логи"""
        pass
    
    def do_GET(self):
        """Обработка GET запросов"""
        try:
            # Парсим URL и query параметры
            parsed_url = urlparse(self.path)
            path = parsed_url.path
            query_params = parse_qs(parsed_url.query)
            
            if path == "/start":
                # Получаем параметры из query string
                username = query_params.get('username', [None])[0]
                appname = query_params.get('appname', [None])[0]
                
                if not username or not appname:
                    self._send_json_response(400, {
                        "error": "Missing required parameters: username, appname"
                    })
                    return
                
                # Запускаем туннель
                result = self.server.tunnel_manager.start(username, appname)
                self._send_json_response(200, result)
            
            elif path == "/stop":
                result = self.server.tunnel_manager.stop()
                self._send_json_response(200, result)
            
            elif path == "/status":
                result = self.server.tunnel_manager.get_status()
                self._send_json_response(200, result)
            
            elif path == "/ping":
                self._send_json_response(200, {"status": "ok"})
            
            else:
                self._send_json_response(404, {"error": "Not found"})
        
        except Exception as e:
            self._send_json_response(500, {"error": str(e)})
    
    def _send_json_response(self, code, data):
        """Отправляет JSON ответ"""
        response = json.dumps(data, ensure_ascii=False).encode('utf-8')
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)


class ControlHTTPServer(HTTPServer):
    """HTTP сервер с доступом к TunnelManager"""
    
    def __init__(self, server_address, RequestHandlerClass, tunnel_manager):
        super().__init__(server_address, RequestHandlerClass)
        self.tunnel_manager = tunnel_manager


class RMService(win32serviceutil.ServiceFramework):
    """Служба Windows для управления туннелем"""
    
    _svc_name_ = SERVICE_NAME
    _svc_display_name_ = SERVICE_DISPLAY_NAME
    _svc_description_ = "Управляет туннелем RocketMan через HTTP API"

    def __init__(self, args):
        win32serviceutil.ServiceFramework.__init__(self, args)
        self.stop_event = win32event.CreateEvent(None, 0, 0, None)
        self.is_alive = True
        
        # Инициализация компонентов службы
        self.tunnel_manager = TunnelManager()
        self.app_monitor = AppMonitor(
            self.tunnel_manager,
            APP_PING_URL,
            APP_CHECK_INTERVAL
        )
        self.http_server = None
        self.http_thread = None

    def SvcStop(self):
        """Остановка службы"""
        self.ReportServiceStatus(win32service.SERVICE_STOP_PENDING)
        servicemanager.LogInfoMsg(f"{SERVICE_NAME}: Stopping service...")
        
        self.is_alive = False
        
        # Останавливаем мониторинг
        try:
            self.app_monitor.stop()
        except Exception as e:
            servicemanager.LogErrorMsg(f"{SERVICE_NAME}: Error stopping monitor: {e}")
        
        # Останавливаем туннель
        try:
            self.tunnel_manager.stop()
        except Exception as e:
            servicemanager.LogErrorMsg(f"{SERVICE_NAME}: Error stopping tunnel: {e}")
        
        # Останавливаем HTTP сервер
        if self.http_server:
            try:
                self.http_server.shutdown()
            except Exception as e:
                servicemanager.LogErrorMsg(f"{SERVICE_NAME}: Error stopping HTTP server: {e}")
        
        # Сигнализируем о завершении
        win32event.SetEvent(self.stop_event)
        servicemanager.LogInfoMsg(f"{SERVICE_NAME}: Service stopped")

    def SvcDoRun(self):
        """Основной цикл службы"""
        servicemanager.LogInfoMsg(f"{SERVICE_NAME}: Service starting...")
        
        try:
            # Запускаем HTTP сервер в отдельном потоке
            self.http_server = ControlHTTPServer(
                ("127.0.0.1", HTTP_PORT),
                ControlHTTPHandler,
                self.tunnel_manager
            )
            
            self.http_thread = threading.Thread(
                target=self._run_http_server,
                daemon=True,
                name="HTTPServerThread"
            )
            self.http_thread.start()
            
            # Ждем, пока сервер не станет доступен
            if not self._wait_for_server_ready(timeout=10):
                raise Exception(f"HTTP server failed to start on port {HTTP_PORT}")
            
            # Запускаем мониторинг приложения
            self.app_monitor.start()
            
            # Сообщаем SCM, что служба запущена
            self.ReportServiceStatus(win32service.SERVICE_RUNNING)
            servicemanager.LogInfoMsg(f"{SERVICE_NAME}: Service started successfully on port {HTTP_PORT}")
            
            # Основной цикл - просто ждем сигнала остановки
            while self.is_alive:
                rc = win32event.WaitForSingleObject(self.stop_event, 5000)
                if rc == win32event.WAIT_OBJECT_0:
                    break
        
        except Exception as e:
            servicemanager.LogErrorMsg(f"{SERVICE_NAME}: Fatal error: {e}")
            self.SvcStop()

    def _run_http_server(self):
        """Запуск HTTP сервера (выполняется в отдельном потоке)"""
        try:
            self.http_server.serve_forever()
        except Exception as e:
            if self.is_alive:
                servicemanager.LogErrorMsg(f"{SERVICE_NAME}: HTTP server error: {e}")

    def _wait_for_server_ready(self, timeout=10):
        """Ожидание готовности HTTP сервера"""
        start_time = time.time()
        
        while time.time() - start_time < timeout:
            try:
                sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                sock.settimeout(1)
                result = sock.connect_ex(("127.0.0.1", HTTP_PORT))
                sock.close()
                
                if result == 0:
                    return True
            except:
                pass
            
            time.sleep(0.2)
        
        return False


def main():
    """Точка входа"""
    if len(sys.argv) == 1:
        servicemanager.Initialize()
        servicemanager.PrepareToHostSingle(RMService)
        servicemanager.StartServiceCtrlDispatcher()
    else:
        win32serviceutil.HandleCommandLine(RMService)


if __name__ == "__main__":
    main()