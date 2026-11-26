find_package(Threads REQUIRED)

add_executable(test_json_util
    test_json_util.cpp
    "${CMAKE_CURRENT_SOURCE_DIR}/../json_util.cpp"
)
target_include_directories(test_json_util PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}/..")
target_link_libraries(test_json_util PRIVATE nlohmann_json::nlohmann_json)
add_test(NAME test_json_util COMMAND test_json_util)

add_executable(test_socket_client
    test_socket_client.cpp
    "${CMAKE_CURRENT_SOURCE_DIR}/../socket_client.cpp"
)
target_include_directories(test_socket_client PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}/..")
target_link_libraries(test_socket_client PRIVATE Threads::Threads)
add_test(NAME test_socket_client COMMAND test_socket_client)

add_executable(test_engine_integration
    test_engine_integration.cpp
    "${CMAKE_CURRENT_SOURCE_DIR}/../json_util.cpp"
)
target_include_directories(test_engine_integration PRIVATE "${CMAKE_CURRENT_SOURCE_DIR}/..")
target_link_libraries(test_engine_integration PRIVATE
    Fcitx5::Core
    Fcitx5::Config
    Fcitx5::Utils
    nlohmann_json::nlohmann_json
)
target_compile_definitions(test_engine_integration PRIVATE
    BUILD_ADDON_SO="$<TARGET_FILE:gemini-voice>"
    BUILD_ADDON_CONF="${CMAKE_BINARY_DIR}/gemini-voice-addon.conf"
)
add_dependencies(test_engine_integration gemini-voice)
add_test(NAME test_engine_integration COMMAND test_engine_integration)
