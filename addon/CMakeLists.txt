cmake_minimum_required(VERSION 3.16)
project(fcitx5-gemini-voice VERSION 0.1.0 LANGUAGES CXX)

find_package(Fcitx5Core REQUIRED)
include("${FCITX_INSTALL_CMAKECONFIG_DIR}/Fcitx5Utils/Fcitx5CompilerSettings.cmake")

option(ENABLE_TEST "Enable unit and integration tests" ON)

add_fcitx5_addon(gemini-voice
    engine.cpp
    factory.cpp
    json_util.cpp
    socket_client.cpp
)
target_link_libraries(gemini-voice PRIVATE
    Fcitx5::Core
    Fcitx5::Config
    Fcitx5::Utils
)
target_include_directories(gemini-voice PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}")

configure_file(gemini-voice-addon.conf.in
    "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice-addon.conf" @ONLY)
configure_file(gemini-voice.conf.in
    "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice.conf" @ONLY)

install(TARGETS gemini-voice DESTINATION "${FCITX_INSTALL_ADDONDIR}")
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice-addon.conf"
    RENAME gemini-voice.conf
    DESTINATION "${FCITX_INSTALL_PKGDATADIR}/addon")
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/gemini-voice.conf"
    DESTINATION "${FCITX_INSTALL_PKGDATADIR}/inputmethod")

if(ENABLE_TEST)
    enable_testing()
    add_subdirectory(tests)
endif()
