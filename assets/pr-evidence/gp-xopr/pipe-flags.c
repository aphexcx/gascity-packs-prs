/* Diagnostic addon: inspect fd 1 without spawning or changing its flags. */
#include <node_api.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/time.h>

static void number(napi_env env, napi_value obj, const char *name, int value) {
  napi_value result;
  napi_create_int32(env, value, &result);
  napi_set_named_property(env, obj, name, result);
}
static napi_value inspect(napi_env env, napi_callback_info info) {
  napi_value result;
  napi_create_object(env, &result);
  int flags = fcntl(1, F_GETFL);
  struct timeval timeout = {0};
  socklen_t size = sizeof(timeout);
  int status = getsockopt(1, SOL_SOCKET, SO_SNDTIMEO, &timeout, &size);
  number(env, result, "flags", flags);
  number(env, result, "nonblocking", !!(flags & O_NONBLOCK));
  number(env, result, "socket_option_status", status);
  number(env, result, "send_timeout_seconds", (int)timeout.tv_sec);
  number(env, result, "send_timeout_microseconds", (int)timeout.tv_usec);
  return result;
}
NAPI_MODULE_INIT() {
  napi_value fn;
  napi_create_function(env, "inspect", NAPI_AUTO_LENGTH, inspect, NULL, &fn);
  return fn;
}
