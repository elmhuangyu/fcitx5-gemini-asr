cmake_minimum_required(VERSION 3.16)
project(fcitx5-gemini-voice VERSION 0.1.0 LANGUAGES CXX)

find_package(Fcitx5Core REQUIRED)
include("${FCITX_INSTALL_CMAKECONFIG_DIR}/Fcitx5Utils/Fcitx5CompilerSettings.cmake")
find_package(Gettext)

# nlohmann/json: prefer the system package, fall back to a pinned release.
find_package(nlohmann_json 3.2.0 CONFIG QUIET)
if(NOT nlohmann_json_FOUND)
    message(STATUS "System nlohmann_json not found; fetching v3.12.0")
    include(FetchContent)
    FetchContent_Declare(nlohmann_json
        URL https://github.com/nlohmann/json/releases/download/v3.12.0/json.tar.xz
        URL_HASH SHA256=42f6e95cad6ec532fd372391373363b62a14af6d771056dbfc86160e6dfff7aa
        DOWNLOAD_EXTRACT_TIMESTAMP TRUE)
    FetchContent_MakeAvailable(nlohmann_json)
endif()

option(ENABLE_TEST "Enable unit and integration tests" ON)

add_fcitx5_addon(gemini-voice
    engine.cpp
    engine_daemon.cpp
    engine_keys.cpp
    engine_menus.cpp
    engine_panel.cpp
    engine_targets.cpp
    engine_text.cpp
    factory.cpp
    json_util.cpp
    socket_client.cpp
)
target_link_libraries(gemini-voice PRIVATE
    Fcitx5::Core
    Fcitx5::Config
    Fcitx5::Utils
    nlohmann_json::nlohmann_json
)
target_include_directories(gemini-voice PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")
fcitx5_add_i18n_definition(TARGETS gemini-voice)
target_compile_definitions(gemini-voice PRIVATE
    FCITX_GETTEXT_DOMAIN="fcitx5-gemini-voice")

configure_file(gemini-voice-addon.conf.in
    "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice-addon.conf" @ONLY)

install(TARGETS gemini-voice DESTINATION "${FCITX_INSTALL_ADDONDIR}")
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice-addon.conf"
    RENAME gemini-voice.conf
    DESTINATION "${FCITX_INSTALL_PKGDATADIR}/addon")

if(GETTEXT_MSGFMT_EXECUTABLE)
    add_subdirectory(po)
else()
    message(WARNING "msgfmt not found; skipping translation compilation")
endif()

if(ENABLE_TEST)
    enable_testing()
    add_subdirectory(tests)
endif()
