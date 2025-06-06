// Code generated by mockery; DO NOT EDIT.
// github.com/vektra/mockery
// template: testify
// Copyright (c) The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0
//
// Run 'make generate-mocks' to regenerate.

package mocks

import (
	"context"
	"time"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/storage/v2/api/depstore"
	mock "github.com/stretchr/testify/mock"
)

// NewFactory creates a new instance of Factory. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewFactory(t interface {
	mock.TestingT
	Cleanup(func())
}) *Factory {
	mock := &Factory{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}

// Factory is an autogenerated mock type for the Factory type
type Factory struct {
	mock.Mock
}

type Factory_Expecter struct {
	mock *mock.Mock
}

func (_m *Factory) EXPECT() *Factory_Expecter {
	return &Factory_Expecter{mock: &_m.Mock}
}

// CreateDependencyReader provides a mock function for the type Factory
func (_mock *Factory) CreateDependencyReader() (depstore.Reader, error) {
	ret := _mock.Called()

	if len(ret) == 0 {
		panic("no return value specified for CreateDependencyReader")
	}

	var r0 depstore.Reader
	var r1 error
	if returnFunc, ok := ret.Get(0).(func() (depstore.Reader, error)); ok {
		return returnFunc()
	}
	if returnFunc, ok := ret.Get(0).(func() depstore.Reader); ok {
		r0 = returnFunc()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(depstore.Reader)
		}
	}
	if returnFunc, ok := ret.Get(1).(func() error); ok {
		r1 = returnFunc()
	} else {
		r1 = ret.Error(1)
	}
	return r0, r1
}

// Factory_CreateDependencyReader_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'CreateDependencyReader'
type Factory_CreateDependencyReader_Call struct {
	*mock.Call
}

// CreateDependencyReader is a helper method to define mock.On call
func (_e *Factory_Expecter) CreateDependencyReader() *Factory_CreateDependencyReader_Call {
	return &Factory_CreateDependencyReader_Call{Call: _e.mock.On("CreateDependencyReader")}
}

func (_c *Factory_CreateDependencyReader_Call) Run(run func()) *Factory_CreateDependencyReader_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run()
	})
	return _c
}

func (_c *Factory_CreateDependencyReader_Call) Return(reader depstore.Reader, err error) *Factory_CreateDependencyReader_Call {
	_c.Call.Return(reader, err)
	return _c
}

func (_c *Factory_CreateDependencyReader_Call) RunAndReturn(run func() (depstore.Reader, error)) *Factory_CreateDependencyReader_Call {
	_c.Call.Return(run)
	return _c
}

// NewReader creates a new instance of Reader. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewReader(t interface {
	mock.TestingT
	Cleanup(func())
}) *Reader {
	mock := &Reader{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}

// Reader is an autogenerated mock type for the Reader type
type Reader struct {
	mock.Mock
}

type Reader_Expecter struct {
	mock *mock.Mock
}

func (_m *Reader) EXPECT() *Reader_Expecter {
	return &Reader_Expecter{mock: &_m.Mock}
}

// GetDependencies provides a mock function for the type Reader
func (_mock *Reader) GetDependencies(ctx context.Context, query depstore.QueryParameters) ([]model.DependencyLink, error) {
	ret := _mock.Called(ctx, query)

	if len(ret) == 0 {
		panic("no return value specified for GetDependencies")
	}

	var r0 []model.DependencyLink
	var r1 error
	if returnFunc, ok := ret.Get(0).(func(context.Context, depstore.QueryParameters) ([]model.DependencyLink, error)); ok {
		return returnFunc(ctx, query)
	}
	if returnFunc, ok := ret.Get(0).(func(context.Context, depstore.QueryParameters) []model.DependencyLink); ok {
		r0 = returnFunc(ctx, query)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).([]model.DependencyLink)
		}
	}
	if returnFunc, ok := ret.Get(1).(func(context.Context, depstore.QueryParameters) error); ok {
		r1 = returnFunc(ctx, query)
	} else {
		r1 = ret.Error(1)
	}
	return r0, r1
}

// Reader_GetDependencies_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'GetDependencies'
type Reader_GetDependencies_Call struct {
	*mock.Call
}

// GetDependencies is a helper method to define mock.On call
//   - ctx context.Context
//   - query depstore.QueryParameters
func (_e *Reader_Expecter) GetDependencies(ctx interface{}, query interface{}) *Reader_GetDependencies_Call {
	return &Reader_GetDependencies_Call{Call: _e.mock.On("GetDependencies", ctx, query)}
}

func (_c *Reader_GetDependencies_Call) Run(run func(ctx context.Context, query depstore.QueryParameters)) *Reader_GetDependencies_Call {
	_c.Call.Run(func(args mock.Arguments) {
		var arg0 context.Context
		if args[0] != nil {
			arg0 = args[0].(context.Context)
		}
		var arg1 depstore.QueryParameters
		if args[1] != nil {
			arg1 = args[1].(depstore.QueryParameters)
		}
		run(
			arg0,
			arg1,
		)
	})
	return _c
}

func (_c *Reader_GetDependencies_Call) Return(dependencyLinks []model.DependencyLink, err error) *Reader_GetDependencies_Call {
	_c.Call.Return(dependencyLinks, err)
	return _c
}

func (_c *Reader_GetDependencies_Call) RunAndReturn(run func(ctx context.Context, query depstore.QueryParameters) ([]model.DependencyLink, error)) *Reader_GetDependencies_Call {
	_c.Call.Return(run)
	return _c
}

// NewWriter creates a new instance of Writer. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewWriter(t interface {
	mock.TestingT
	Cleanup(func())
}) *Writer {
	mock := &Writer{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}

// Writer is an autogenerated mock type for the Writer type
type Writer struct {
	mock.Mock
}

type Writer_Expecter struct {
	mock *mock.Mock
}

func (_m *Writer) EXPECT() *Writer_Expecter {
	return &Writer_Expecter{mock: &_m.Mock}
}

// WriteDependencies provides a mock function for the type Writer
func (_mock *Writer) WriteDependencies(ts time.Time, dependencies []model.DependencyLink) error {
	ret := _mock.Called(ts, dependencies)

	if len(ret) == 0 {
		panic("no return value specified for WriteDependencies")
	}

	var r0 error
	if returnFunc, ok := ret.Get(0).(func(time.Time, []model.DependencyLink) error); ok {
		r0 = returnFunc(ts, dependencies)
	} else {
		r0 = ret.Error(0)
	}
	return r0
}

// Writer_WriteDependencies_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'WriteDependencies'
type Writer_WriteDependencies_Call struct {
	*mock.Call
}

// WriteDependencies is a helper method to define mock.On call
//   - ts time.Time
//   - dependencies []model.DependencyLink
func (_e *Writer_Expecter) WriteDependencies(ts interface{}, dependencies interface{}) *Writer_WriteDependencies_Call {
	return &Writer_WriteDependencies_Call{Call: _e.mock.On("WriteDependencies", ts, dependencies)}
}

func (_c *Writer_WriteDependencies_Call) Run(run func(ts time.Time, dependencies []model.DependencyLink)) *Writer_WriteDependencies_Call {
	_c.Call.Run(func(args mock.Arguments) {
		var arg0 time.Time
		if args[0] != nil {
			arg0 = args[0].(time.Time)
		}
		var arg1 []model.DependencyLink
		if args[1] != nil {
			arg1 = args[1].([]model.DependencyLink)
		}
		run(
			arg0,
			arg1,
		)
	})
	return _c
}

func (_c *Writer_WriteDependencies_Call) Return(err error) *Writer_WriteDependencies_Call {
	_c.Call.Return(err)
	return _c
}

func (_c *Writer_WriteDependencies_Call) RunAndReturn(run func(ts time.Time, dependencies []model.DependencyLink) error) *Writer_WriteDependencies_Call {
	_c.Call.Return(run)
	return _c
}
